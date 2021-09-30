package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"

	"github.com/alitto/pond"
	"github.com/containers/image/v5/docker/reference"
	"github.com/containers/image/v5/transports/alltransports"
	ctypes "github.com/containers/image/v5/types"
	"github.com/estahn/k8s-image-swapper/pkg/config"
	"github.com/estahn/k8s-image-swapper/pkg/registry"
	"github.com/estahn/k8s-image-swapper/pkg/secrets"
	types "github.com/estahn/k8s-image-swapper/pkg/types"
	jmespath "github.com/jmespath/go-jmespath"
	"github.com/rs/zerolog/log"
	kwhmodel "github.com/slok/kubewebhook/v2/pkg/model"
	"github.com/slok/kubewebhook/v2/pkg/webhook"
	kwhmutating "github.com/slok/kubewebhook/v2/pkg/webhook/mutating"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

var execCommand = exec.Command

// Option represents an option that can be passed when instantiating the image swapper to customize it
type Option func(*ImageSwapper)

// ImagePullSecretsProvider allows to pass a provider reading out Kubernetes secrets
func ImagePullSecretsProvider(provider secrets.ImagePullSecretsProvider) Option {
	return func(swapper *ImageSwapper) {
		swapper.imagePullSecretProvider = provider
	}
}

// Filters allows to pass JMESPathFilter to select the images to be swapped
func Filters(filters []config.JMESPathFilter) Option {
	return func(swapper *ImageSwapper) {
		swapper.filters = filters
	}
}

// ImageSwapPolicy allows to pass the ImageSwapPolicy option
func ImageSwapPolicy(policy types.ImageSwapPolicy) Option {
	return func(swapper *ImageSwapper) {
		swapper.imageSwapPolicy = policy
	}
}

// ImageCopyPolicy allows to pass the ImageCopyPolicy option
func ImageCopyPolicy(policy types.ImageCopyPolicy) Option {
	return func(swapper *ImageSwapper) {
		swapper.imageCopyPolicy = policy
	}
}

// Copier allows to pass the copier option
func Copier(pool *pond.WorkerPool) Option {
	return func(swapper *ImageSwapper) {
		swapper.copier = pool
	}
}

// ImageSwapper is a mutator that will download images and change the image name.
type ImageSwapper struct {
	registryClient          registry.Client
	imagePullSecretProvider secrets.ImagePullSecretsProvider

	// filters defines a list of expressions to remove objects that should not be processed,
	// by default all objects will be processed
	filters []config.JMESPathFilter

	// copier manages the jobs copying the images to the target registry
	copier *pond.WorkerPool

	imageSwapPolicy types.ImageSwapPolicy
	imageCopyPolicy types.ImageCopyPolicy
}

// NewImageSwapper returns a new ImageSwapper initialized.
func NewImageSwapper(registryClient registry.Client, imagePullSecretProvider secrets.ImagePullSecretsProvider, filters []config.JMESPathFilter, imageSwapPolicy types.ImageSwapPolicy, imageCopyPolicy types.ImageCopyPolicy) kwhmutating.Mutator {
	return &ImageSwapper{
		registryClient:          registryClient,
		imagePullSecretProvider: imagePullSecretProvider,
		filters:                 filters,
		copier:                  pond.New(100, 1000),
		imageSwapPolicy:         imageSwapPolicy,
		imageCopyPolicy:         imageCopyPolicy,
	}
}

// NewImageSwapperWithOpts returns a configured ImageSwapper instance
func NewImageSwapperWithOpts(registryClient registry.Client, opts ...Option) kwhmutating.Mutator {
	swapper := &ImageSwapper{
		registryClient:          registryClient,
		imagePullSecretProvider: secrets.NewDummyImagePullSecretsProvider(),
		filters:                 []config.JMESPathFilter{},
		imageSwapPolicy:         types.ImageSwapPolicyExists,
		imageCopyPolicy:         types.ImageCopyPolicyDelayed,
	}

	for _, opt := range opts {
		opt(swapper)
	}

	// Initialise worker pool if not configured
	if swapper.copier == nil {
		swapper.copier = pond.New(100, 1000)
	}

	return swapper
}

func NewImageSwapperWebhookWithOpts(registryClient registry.Client, opts ...Option) (webhook.Webhook, error) {
	imageSwapper := NewImageSwapperWithOpts(registryClient, opts...)
	mt := kwhmutating.MutatorFunc(imageSwapper.Mutate)
	mcfg := kwhmutating.WebhookConfig{
		ID:      "k8s-image-swapper",
		Obj:     &corev1.Pod{},
		Mutator: mt,
	}

	return kwhmutating.NewWebhook(mcfg)
}

func NewImageSwapperWebhook(registryClient registry.Client, imagePullSecretProvider secrets.ImagePullSecretsProvider, filters []config.JMESPathFilter, imageSwapPolicy types.ImageSwapPolicy, imageCopyPolicy types.ImageCopyPolicy) (webhook.Webhook, error) {
	imageSwapper := NewImageSwapper(registryClient, imagePullSecretProvider, filters, imageSwapPolicy, imageCopyPolicy)
	mt := kwhmutating.MutatorFunc(imageSwapper.Mutate)
	mcfg := kwhmutating.WebhookConfig{
		ID:      "k8s-image-swapper",
		Obj:     &corev1.Pod{},
		Mutator: mt,
	}

	return kwhmutating.NewWebhook(mcfg)
}

// Mutate replaces the image ref. Satisfies mutating.Mutator interface.
func (p *ImageSwapper) Mutate(ctx context.Context, ar *kwhmodel.AdmissionReview, obj metav1.Object) (*kwhmutating.MutatorResult, error) {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return &kwhmutating.MutatorResult{}, nil
	}

	logger := log.With().
		Str("uid", string(ar.ID)).
		Str("kind", ar.RequestGVK.String()).
		Str("namespace", ar.Namespace).
		Str("name", pod.Name).
		Logger()

	lctx := logger.
		WithContext(ctx)

	for i, container := range pod.Spec.Containers {
		srcRef, err := alltransports.ParseImageName("docker://" + container.Image)
		if err != nil {
			log.Ctx(lctx).Warn().Msgf("invalid source name %s: %v", container.Image, err)
			continue
		}

		// skip if the source and target registry domain are equal (e.g. same ECR registries)
		if domain := reference.Domain(srcRef.DockerReference()); domain == p.registryClient.Endpoint() {
			continue
		}

		filterCtx := NewFilterContext(*ar, pod, container)
		if filterMatch(filterCtx, p.filters) {
			log.Ctx(lctx).Debug().Msg("skip due to filter condition")
			continue
		}

		targetImage := p.targetName(srcRef)

		copyFn := func() {
			// Avoid unnecessary copying by ending early. For images such as :latest we adhere to the
			// image pull policy.
			if p.registryClient.ImageExists(targetImage) && container.ImagePullPolicy != corev1.PullAlways {
				return
			}

			// Create repository
			createRepoName := reference.TrimNamed(srcRef.DockerReference()).String()
			log.Ctx(lctx).Debug().Str("repository", createRepoName).Msg("create repository")
			if err := p.registryClient.CreateRepository(createRepoName); err != nil {
				log.Err(err)
			}

			// Retrieve secrets and auth credentials
			imagePullSecrets, err := p.imagePullSecretProvider.GetImagePullSecrets(pod)
			if err != nil {
				log.Err(err)
			}

			authFile, err := imagePullSecrets.AuthFile()
			if authFile != nil {
				defer func() {
					if err := os.RemoveAll(authFile.Name()); err != nil {
						log.Err(err)
					}
				}()
			}

			if err != nil {
				log.Err(err)
			}

			// Copy image
			// TODO: refactor to use structure instead of passing file name / string
			//       or transform registryClient creds into auth compatible form, e.g.
			//       {"auths":{"aws_account_id.dkr.ecr.region.amazonaws.com":{"username":"AWS","password":"..."	}}}
			log.Ctx(lctx).Trace().Str("source", srcRef.DockerReference().String()).Str("target", targetImage).Msg("copy image")
			if err := copyImage(srcRef.DockerReference().String(), authFile.Name(), targetImage, p.registryClient.Credentials()); err != nil {
				log.Ctx(lctx).Err(err).Str("source", srcRef.DockerReference().String()).Str("target", targetImage).Msg("copying image to target registry failed")
			}
		}

		// imageCopyPolicy
		switch p.imageCopyPolicy {
		case types.ImageCopyPolicyDelayed:
			p.copier.Submit(copyFn)
		case types.ImageCopyPolicyImmediate:
			// TODO: Implement deadline
			p.copier.SubmitAndWait(copyFn)
		case types.ImageCopyPolicyForce:
			// TODO: Implement deadline
			copyFn()
		default:
			panic("unknown imageCopyPolicy")
		}

		// imageSwapPolicy
		switch p.imageSwapPolicy {
		case types.ImageSwapPolicyAlways:
			log.Ctx(lctx).Debug().Str("image", targetImage).Msg("set new container image")
			pod.Spec.Containers[i].Image = targetImage
		case types.ImageSwapPolicyExists:
			if p.registryClient.ImageExists(targetImage) {
				log.Ctx(lctx).Debug().Str("image", targetImage).Msg("set new container image")
				pod.Spec.Containers[i].Image = targetImage
			} else {
				log.Ctx(lctx).Debug().Str("image", targetImage).Msg("container image not found in target registry, not swapping")
			}
		default:
			panic("unknown imageSwapPolicy")
		}
	}

	return &kwhmutating.MutatorResult{MutatedObject: pod}, nil
}

// filterMatch returns true if one of the filters matches the context
func filterMatch(ctx FilterContext, filters []config.JMESPathFilter) bool {
	// Simplify FilterContext to be easier searchable by marshaling it to JSON and back to an interface
	var filterContext interface{}
	jsonBlob, err := json.Marshal(ctx)
	if err != nil {
		log.Err(err).Msg("could not marshal filter context")
		return false
	}

	err = json.Unmarshal(jsonBlob, &filterContext)
	if err != nil {
		log.Err(err).Msg("could not unmarshal json blob")
		return false
	}

	log.Debug().Interface("object", filterContext).Msg("generated filter context")

	for idx, filter := range filters {
		results, err := jmespath.Search(filter.JMESPath, filterContext)
		log.Debug().Str("filter", filter.JMESPath).Interface("results", results).Msg("jmespath search results")

		if err != nil {
			log.Err(err).Str("filter", filter.JMESPath).Msgf("Filter (idx %v) could not be evaluated.", idx)
			return false
		}

		switch results.(type) {
		case bool:
			if results == true {
				return true
			}
		default:
			log.Warn().Str("filter", filter.JMESPath).Msg("filter does not return a bool value")
		}
	}

	return false
}

// targetName returns the reference in the target repository
func (p *ImageSwapper) targetName(ref ctypes.ImageReference) string {
	return fmt.Sprintf("%s/%s", p.registryClient.Endpoint(), ref.DockerReference().String())
}

// FilterContext is being used by JMESPath to search and match
type FilterContext struct {
	// Obj contains the object submitted to the webhook (currently only pods)
	Obj metav1.Object `json:"obj,omitempty"`

	// Container contains the currently processed container
	Container corev1.Container `json:"container,omitempty"`
}

func NewFilterContext(request kwhmodel.AdmissionReview, obj metav1.Object, container corev1.Container) FilterContext {
	if obj.GetNamespace() == "" {
		obj.SetNamespace(request.Namespace)
	}

	return FilterContext{Obj: obj, Container: container}
}

func copyImage(src string, srcCeds string, dest string, destCreds string) error {
	app := "skopeo"
	args := []string{
		"--override-os", "linux",
		"copy",
		"--retry-times", "3",
		"docker://" + src,
		"docker://" + dest,
	}

	if len(srcCeds) > 0 {
		args = append(args, "--src-authfile", srcCeds)
	} else {
		args = append(args, "--src-no-creds")
	}

	if len(destCreds) > 0 {
		args = append(args, "--dest-creds", destCreds)
	} else {
		args = append(args, "--dest-no-creds")
	}

	cmd := execCommand(app, args...)
	output, err := cmd.CombinedOutput()

	log.Trace().
		Str("app", app).
		Strs("args", args).
		Bytes("output", output).
		Msg("executed command to copy image")

	return err
}
