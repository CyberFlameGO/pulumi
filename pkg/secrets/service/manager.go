// Copyright 2016-2022, Pulumi Corporation.
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

// Package service implements support for the Pulumi Service secret manager.
package service

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"

	"github.com/pulumi/pulumi/pkg/v3/backend/httpstate/client"
	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
)

const Type = "service"

// serviceCrypter is an encrypter/decrypter that uses the Pulumi servce to encrypt/decrypt a stack's secrets.
type serviceCrypter struct {
	client *client.Client
	stack  client.StackIdentifier
}

func newServiceCrypter(client *client.Client, stack client.StackIdentifier) config.Crypter {
	return &serviceCrypter{client: client, stack: stack}
}

func (c *serviceCrypter) EncryptValue(ctx context.Context, plaintext string) (string, error) {
	ciphertext, err := c.client.EncryptValue(ctx, c.stack, []byte(plaintext))
	if err != nil {
		return "", err
	}
	return base64.StdEncoding.EncodeToString(ciphertext), nil
}

func (c *serviceCrypter) DecryptValue(ctx context.Context, cipherstring string) (string, error) {
	ciphertext, err := base64.StdEncoding.DecodeString(cipherstring)
	if err != nil {
		return "", err
	}

	plaintext, err := c.client.DecryptValue(ctx, c.stack, ciphertext)
	if err != nil {
		return "", err
	}
	return string(plaintext), nil
}

func (c *serviceCrypter) BulkDecrypt(ctx context.Context, secrets []string) (map[string]string, error) {
	var secretsToDecrypt = make([][]byte, 0, len(secrets))
	for _, val := range secrets {
		ciphertext, err := base64.StdEncoding.DecodeString(val)
		if err != nil {
			return nil, err
		}
		secretsToDecrypt = append(secretsToDecrypt, ciphertext)
	}

	decryptedList, err := c.client.BulkDecryptValue(ctx, c.stack, secretsToDecrypt)
	if err != nil {
		return nil, err
	}

	decryptedSecrets := make(map[string]string)
	for name, val := range decryptedList {
		decryptedSecrets[name] = string(val)
	}

	return decryptedSecrets, nil
}

type serviceSecretsManagerState struct {
	URL     string `json:"url,omitempty"`
	Owner   string `json:"owner"`
	Project string `json:"project"`
	Stack   string `json:"stack"`
}

var _ secrets.Manager = &serviceSecretsManager{}

type serviceSecretsManager struct {
	state   serviceSecretsManagerState
	crypter config.Crypter
}

func (sm *serviceSecretsManager) Type() string {
	return Type
}

func (sm *serviceSecretsManager) State() interface{} {
	return sm.state
}

func (sm *serviceSecretsManager) Decrypter() (config.Decrypter, error) {
	contract.Assert(sm.crypter != nil)
	return sm.crypter, nil
}

func (sm *serviceSecretsManager) Encrypter() (config.Encrypter, error) {
	contract.Assert(sm.crypter != nil)
	return sm.crypter, nil
}

func NewServiceSecretsManager(
	client *client.Client, id client.StackIdentifier, stackName tokens.Name, configFile string) (secrets.Manager, error) {

	contract.Assertf(stackName != "", "stackName %s", "!= \"\"")

	project, _, err := workspace.DetectProjectStackPath(stackName.Q())
	if err != nil {
		return nil, err
	}

	info, err := workspace.LoadProjectStack(project, configFile)
	if err != nil {
		return nil, err
	}

	// We should only save the ProjectStack at this point IF we have changed the
	// secrets provider. To change the secrets provider to a serviceSecretsManager
	// we would need to ensure that there are no remnants of the old secret manager
	// To remove those remnants, we would set those values to be empty in the project
	// stack, as per changeProjectStackSecretDetails func.
	// If we do not check to see if the secrets provider has changed, then we will actually
	// reload the configuration file to be sorted or an empty {} when creating a stack
	// this is not the desired behaviour.
	if changeProjectStackSecretDetails(info) {
		if err := workspace.SaveProjectStack(stackName.Q(), info); err != nil {
			return nil, err
		}
	}

	return &serviceSecretsManager{
		state: serviceSecretsManagerState{
			URL:     client.URL(),
			Owner:   id.Owner,
			Project: id.Project,
			Stack:   id.Stack,
		},
		crypter: newServiceCrypter(client, id),
	}, nil
}

// NewServiceSecretsManagerFromState returns a Pulumi service-based secrets manager based on the
// existing state.
func NewServiceSecretsManagerFromState(state json.RawMessage) (secrets.Manager, error) {
	var s serviceSecretsManagerState
	if err := json.Unmarshal(state, &s); err != nil {
		return nil, fmt.Errorf("unmarshalling state: %w", err)
	}

	account, err := workspace.GetAccount(s.URL)
	if err != nil {
		return nil, fmt.Errorf("getting access token: %w", err)
	}
	token := account.AccessToken

	if token == "" {
		return nil, fmt.Errorf("could not find access token for %s, have you logged in?", s.URL)
	}

	id := client.StackIdentifier{
		Owner:   s.Owner,
		Project: s.Project,
		Stack:   s.Stack,
	}
	c := client.NewClient(s.URL, token, diag.DefaultSink(io.Discard, io.Discard, diag.FormatOptions{
		Color: colors.Never}))

	return &serviceSecretsManager{
		state:   s,
		crypter: newServiceCrypter(c, id),
	}, nil
}

// A passphrase secrets provider has an encryption salt, therefore, changing
// from passphrase to serviceSecretsManager requires the encryption salt
// to be removed.
// A cloud secrets manager has an encryption key and a secrets provider,
// therefore, changing from cloud to serviceSecretsManager requires the
// encryption key and secrets provider to be removed.
// Regardless of what the current secrets provider is, all of these values
// need to be empty otherwise `getStackSecretsManager` in crypto.go can
// potentially return the incorrect secret type for the stack.
func changeProjectStackSecretDetails(info *workspace.ProjectStack) bool {
	var requiresSave bool
	if info.SecretsProvider != "" {
		info.SecretsProvider = ""
		requiresSave = true
	}
	if info.EncryptedKey != "" {
		info.EncryptedKey = ""
		requiresSave = true
	}
	if info.EncryptionSalt != "" {
		info.EncryptionSalt = ""
		requiresSave = true
	}
	return requiresSave
}
