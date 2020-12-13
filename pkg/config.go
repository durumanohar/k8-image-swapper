/*
Copyright © 2020 Enrico Stahn <enrico.stahn@gmail.com>

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in
all copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN
THE SOFTWARE.
*/
package pkg

import "fmt"

type Config struct {
	LogLevel string `yaml:"logFormat" validate:"oneof=debug info warn error fatal"`
	LogFormat string `yaml:"logFormat" validate:"oneof=json console"`

	ListenAddress string

	DryRun bool `yaml:"dryRun"`
	Source Source `yaml:"source"`
	Target AWS

	TLSCertFile string
	TLSKeyFile string
}

type Source struct {
	Filters []JMESPathFilter `yaml:"filters"`
}

type JMESPathFilter struct {
	JMESPath string `yaml:"jmespath"`
}

type AWS struct {
	AccountID string `yaml:"accountId"`
	Region string `yaml:"region"`
}

func (a *AWS) EcrDomain() string {
	return fmt.Sprintf("%s.dkr.ecr.%s.amazonaws.com", a.AccountID, a.Region)
}
