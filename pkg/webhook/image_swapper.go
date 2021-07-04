package webhook

import (
	"context"
	"encoding/json"
	"fmt"
	"os/exec"

	"github.com/alitto/pond"
	"github.com/containers/image/v5/transports/alltransports"
	ctypes "github.com/containers/image/v5/types"
	"github.com/estahn/k8s-image-swapper/pkg/config"
	"github.com/estahn/k8s-image-swapper/pkg/registry"
	types "github.com/estahn/k8s-image-swapper/pkg/types"
	"github.com/jmespath/go-jmespath"
	"github.com/rs/zerolog/log"
	"github.com/slok/kubewebhook/pkg/webhook"
	whcontext "github.com/slok/kubewebhook/pkg/webhook/context"
	"k8s.io/api/admission/v1beta1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	metaV1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	coreV1Types "k8s.io/client-go/kubernetes/typed/core/v1"
	"k8s.io/client-go/rest"

	"github.com/containers/image/v5/docker/reference"
	"github.com/slok/kubewebhook/pkg/webhook/mutating"
)

// ImageSwapper is a mutator that will download images and change the image name.
type ImageSwapper struct {
	registryClient registry.Client

	// filters defines a list of expressions to remove objects that should not be processed,
	// by default all objects will be processed
	filters []config.JMESPathFilter

	// copier manages the jobs copying the images to the target registry
	copier *pond.WorkerPool

	imageSwapPolicy types.ImageSwapPolicy
	imageCopyPolicy types.ImageCopyPolicy
}

// NewImageSwapper returns a new ImageSwapper initialized.
func NewImageSwapper(registryClient registry.Client, filters []config.JMESPathFilter, imageSwapPolicy types.ImageSwapPolicy, imageCopyPolicy types.ImageCopyPolicy) mutating.Mutator {
	return &ImageSwapper{
		registryClient:  registryClient,
		filters:         filters,
		copier:          pond.New(100, 1000),
		imageSwapPolicy: imageSwapPolicy,
		imageCopyPolicy: imageCopyPolicy,
	}
}

func NewImageSwapperWebhook(registryClient registry.Client, filters []config.JMESPathFilter, imageSwapPolicy types.ImageSwapPolicy, imageCopyPolicy types.ImageCopyPolicy) (webhook.Webhook, error) {
	imageSwapper := NewImageSwapper(registryClient, filters, imageSwapPolicy, imageCopyPolicy)
	mt := mutating.MutatorFunc(imageSwapper.Mutate)
	mcfg := mutating.WebhookConfig{
		Name: "k8s-image-swapper",
		Obj:  &corev1.Pod{},
	}

	return mutating.NewWebhook(mcfg, mt, nil, nil, nil)
}

// Mutate will set the required labels on the pods. Satisfies mutating.Mutator interface.
func (p *ImageSwapper) Mutate(ctx context.Context, obj metav1.Object) (bool, error) {
	//switch _ := obj.(type) {
	//case *corev1.Pod:
	//	// o is a pod
	//case *v1beta1.Role:
	//	// o is the actual role Object with all fields etc
	//case *v1beta1.RoleBinding:
	//case *v1beta1.ClusterRole:
	//case *v1beta1.ClusterRoleBinding:
	//case *v1.ServiceAccount:
	//default:
	//	//o is unknown for us
	//}

	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return false, nil
	}

	ar := whcontext.GetAdmissionRequest(ctx)
	logger := log.With().
		Str("uid", string(ar.UID)).
		Str("kind", ar.Kind.String()).
		Str("namespace", ar.Namespace).
		Str("name", pod.Name).
		Logger()
	//spew.Dump()

	lctx := logger.
		WithContext(ctx)

	// Use imagePullSecrets to authenticate with source image registry if specfied
	pullSecretsTokens, err := getPullSecretsAuthTokens(pod) //make(map[string]string)
	if err != nil {
		log.Warn().Msgf("Could not use pullSecret for pod %s: %v", pod.Name, err)
	}
	log.Ctx(lctx).Debug().Msgf("%v", pullSecretsTokens)

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
		log.Ctx(lctx).Debug().Msgf("%v", srcRef)

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

			// Copy image
			log.Ctx(lctx).Trace().Str("source", srcRef.DockerReference().String()).Str("target", targetImage).Msg("copy image")
			if err := copyImage(srcRef.DockerReference().String(), "", targetImage, p.registryClient.Credentials()); err != nil {
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
			}
		default:
			panic("unknown imageSwapPolicy")
		}
	}

	return false, nil
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

func NewFilterContext(request v1beta1.AdmissionRequest, obj metav1.Object, container corev1.Container) FilterContext {
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
		"--src-no-creds",
		"--dest-creds", destCreds,
	}

	cmd := exec.Command(app, args...)
	output, err := cmd.CombinedOutput()

	log.Trace().
		Str("app", app).
		Strs("args", args).
		Bytes("output", output).
		Msg("executed command to copy image")

	return err
}

func configK8SClient() (*kubernetes.Clientset, error) {
	// creates the in-cluster config
	config, err := rest.InClusterConfig()
	if err != nil {
		panic(err.Error())
	}
	// creates the clientset
	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		panic(err.Error())
	}

	return clientset, nil
}

func getPullSecretTokens(pullSecret string, namespace string) ([]map[string]string, error) {

	var pullSecretTokens []map[string]string

	K8SClient, err := configK8SClient()
	if err != nil {
		panic(err.Error())
	}

	var secretsClient coreV1Types.SecretInterface
	secretsClient = K8SClient.CoreV1().Secrets(namespace)

	secret, err := secretsClient.Get(pullSecret, metaV1.GetOptions{})
	if err != nil {
		panic(err.Error())
	}

	for key, value := range secret.Data {
		// key is string, value is []byte

		token := make(map[string]string)
		token[key] = string(value)

		pullSecretTokens = append(pullSecretTokens, token)
	}

	return pullSecretTokens, nil
}

func getPullSecretsAuthTokens(pod metav1.Object) (map[string]string, error) {
	pullSecrets := pod.Spec.ImagePullSecrets
	namespace := pod.GetNamespace()

	var allTokens map[string]string

	// Go over pullSecrets in reverse to override latter tokens with former ones. Will do this with secrets inside each pullSecret too.
	for i := len(pullSecrets) - 1; i >= 0; i-- {
		pullSecretTokens, err := getPullSecretTokens(pullSecrets[i], namespace)
		if err != nil {
			panic(err.Error())
		}

		for j := len(pullSecretTokens) - 1; j >= 0; j-- {
			// Add single token to final map
			for registry, token := range pullSecretTokens[j] {
				allTokens[registry] = token
			}
		}
	}

	return allTokens, nil
}
