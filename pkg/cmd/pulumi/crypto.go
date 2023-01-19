// Copyright 2016-2019, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"fmt"
	"strings"

	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/pkg/v3/resource/stack"
	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/pkg/v3/secrets/cloud"
	"github.com/pulumi/pulumi/pkg/v3/secrets/passphrase"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
)

func getStackEncrypter(s backend.Stack) (config.Encrypter, error) {
	sm, err := getStackSecretsManager(s)
	if err != nil {
		return nil, err
	}

	return sm.Encrypter()
}

func getStackDecrypter(s backend.Stack) (config.Decrypter, error) {
	sm, err := getStackSecretsManager(s)
	if err != nil {
		return nil, err
	}

	return sm.Decrypter()
}

func getStackSecretsManager(s backend.Stack) (secrets.Manager, error) {
	project, _, err := readProject()

	if err != nil {
		return nil, err
	}

	ps, err := loadProjectStack(project, s)
	if err != nil {
		return nil, err
	}

	sm, err := func() (secrets.Manager, error) {
		configFile, err := getProjectStackPath(s)
		if err != nil {
			return nil, err
		}

		//nolint:goconst
		if ps.SecretsProvider != passphrase.Type && ps.SecretsProvider != "default" && ps.SecretsProvider != "" {
			return cloud.NewCloudSecretsManager(
				s.Ref().Name(), configFile, ps.SecretsProvider, false /* rotateSecretsProvider */)
		}

		if ps.EncryptionSalt != "" {
			return passphrase.NewPromptingPassphraseSecretsManager(
				s.Ref().Name(), configFile, false /* rotateSecretsProvider */)
		}

		return s.DefaultSecretManager(configFile)
	}()
	if err != nil {
		return nil, err
	}
	return stack.NewCachingSecretsManager(sm), nil
}

func validateSecretsProvider(typ string) error {
	kind := strings.SplitN(typ, ":", 2)[0]
	supportedKinds := []string{"default", "passphrase", "awskms", "azurekeyvault", "gcpkms", "hashivault"}
	for _, supportedKind := range supportedKinds {
		if kind == supportedKind {
			return nil
		}
	}
	return fmt.Errorf("unknown secrets provider type '%s' (supported values: %s)",
		kind,
		strings.Join(supportedKinds, ","))

}
