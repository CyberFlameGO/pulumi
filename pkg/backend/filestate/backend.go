// Copyright 2016-2023, Pulumi Corporation.
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

package filestate

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/uuid"

	user "github.com/tweekmonster/luser"
	"gocloud.dev/blob"
	_ "gocloud.dev/blob/azureblob" // driver for azblob://
	_ "gocloud.dev/blob/fileblob"  // driver for file://
	"gocloud.dev/blob/gcsblob"     // driver for gs://
	_ "gocloud.dev/blob/s3blob"    // driver for s3://
	"gocloud.dev/gcerrors"

	"github.com/pulumi/pulumi/pkg/v3/authhelpers"
	"github.com/pulumi/pulumi/pkg/v3/backend"
	"github.com/pulumi/pulumi/pkg/v3/backend/display"
	"github.com/pulumi/pulumi/pkg/v3/engine"
	"github.com/pulumi/pulumi/pkg/v3/operations"
	"github.com/pulumi/pulumi/pkg/v3/resource/deploy"
	"github.com/pulumi/pulumi/pkg/v3/resource/edit"
	"github.com/pulumi/pulumi/pkg/v3/resource/stack"
	"github.com/pulumi/pulumi/pkg/v3/secrets"
	"github.com/pulumi/pulumi/pkg/v3/util/validation"
	"github.com/pulumi/pulumi/sdk/v3/go/common/apitype"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag"
	"github.com/pulumi/pulumi/sdk/v3/go/common/diag/colors"
	sdkDisplay "github.com/pulumi/pulumi/sdk/v3/go/common/display"
	"github.com/pulumi/pulumi/sdk/v3/go/common/encoding"
	"github.com/pulumi/pulumi/sdk/v3/go/common/resource/config"
	"github.com/pulumi/pulumi/sdk/v3/go/common/tokens"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/cmdutil"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/result"
	"github.com/pulumi/pulumi/sdk/v3/go/common/workspace"
	"gopkg.in/yaml.v3"
)

// PulumiFilestateGzipEnvVar is an env var that must be truthy
// to enable gzip compression when using the filestate backend.
const PulumiFilestateGzipEnvVar = "PULUMI_SELF_MANAGED_STATE_GZIP"

// Backend extends the base backend interface with specific information about local backends.
type Backend interface {
	backend.Backend
	local() // at the moment, no local specific info, so just use a marker function.

	// Upgrade to the latest state store version.
	Upgrade(ctx context.Context) error
}

type localBackend struct {
	d diag.Sink

	// originalURL is the URL provided when the localBackend was initialized, for example
	// "file://~". url is a canonicalized version that should be used when persisting data.
	// (For example, replacing ~ with the home directory, making an absolute path, etc.)
	originalURL string
	url         string

	bucket Bucket
	mutex  sync.Mutex

	lockID string

	gzip bool

	// The current project, if any.
	currentProject *workspace.Project

	// The store controls the layout of stacks in the backend.
	// We use different layouts based on the version of the backend
	// specified in the metadata file.
	// If the metadata file is missing, we use the legacy layout.
	store referenceStore
}

type localBackendReference struct {
	name    tokens.Name
	project tokens.Name

	// Backend that created this reference.
	b *localBackend

	// referenceStore that created this reference.
	//
	// This is necessary because the referenceStore for a backend
	// may change over time.
	store referenceStore
}

func (r *localBackendReference) String() string {
	// If project is blank this is a legacy non-project scoped stack reference, just return the name.
	if r.project == "" {
		return string(r.name)
	}

	// For project scoped references when stringifying backend references,
	// we take the current project (if present) into account.
	// If the project names match, we can elide them.
	if r.b.currentProject != nil && string(r.project) == string(r.b.currentProject.Name) {
		return string(r.name)
	}

	// Else return a new style fully qualified reference.
	return fmt.Sprintf("organization/%s/%s", r.project, r.name)
}

func (r *localBackendReference) Name() tokens.Name {
	return r.name
}

func (r *localBackendReference) Project() tokens.Name {
	return r.project
}

func (r *localBackendReference) FullyQualifiedName() tokens.QName {
	if r.project == "" {
		return r.name.Q()
	}
	return tokens.QName(fmt.Sprintf("organization/%s/%s", r.project, r.name))
}

// Helper methods that delegate to the underlying referenceStore.
func (r *localBackendReference) Validate() error       { return r.store.ValidateReference(r) }
func (r *localBackendReference) StackBasePath() string { return r.store.StackBasePath(r) }
func (r *localBackendReference) HistoryDir() string    { return r.store.HistoryDir(r) }
func (r *localBackendReference) BackupDir() string     { return r.store.BackupDir(r) }

func IsFileStateBackendURL(urlstr string) bool {
	u, err := url.Parse(urlstr)
	if err != nil {
		return false
	}

	return blob.DefaultURLMux().ValidBucketScheme(u.Scheme)
}

const FilePathPrefix = "file://"

func New(ctx context.Context, d diag.Sink, originalURL string, project *workspace.Project) (Backend, error) {
	if !IsFileStateBackendURL(originalURL) {
		return nil, fmt.Errorf("local URL %s has an illegal prefix; expected one of: %s",
			originalURL, strings.Join(blob.DefaultURLMux().BucketSchemes(), ", "))
	}

	u, err := massageBlobPath(originalURL)
	if err != nil {
		return nil, err
	}

	p, err := url.Parse(u)
	if err != nil {
		return nil, err
	}

	blobmux := blob.DefaultURLMux()

	// for gcp we want to support additional credentials
	// schemes on top of go-cloud's default credentials mux.
	if p.Scheme == gcsblob.Scheme {
		blobmux, err = authhelpers.GoogleCredentialsMux(ctx)
		if err != nil {
			return nil, err
		}
	}

	bucket, err := blobmux.OpenBucket(ctx, u)
	if err != nil {
		return nil, fmt.Errorf("unable to open bucket %s: %w", u, err)
	}

	if !strings.HasPrefix(u, FilePathPrefix) {
		bucketSubDir := strings.TrimLeft(p.Path, "/")
		if bucketSubDir != "" {
			if !strings.HasSuffix(bucketSubDir, "/") {
				bucketSubDir += "/"
			}

			bucket = blob.PrefixedBucket(bucket, bucketSubDir)
		}
	}

	// Check if there is a .pulumi/Pulumi.yaml file in the bucket
	b := &wrappedBucket{bucket: bucket}
	pulumiState, err := ensurePulumiMeta(ctx, b)
	if err != nil {
		return nil, err
	}

	// Allocate a unique lock ID for this backend instance.
	lockID, err := uuid.NewV4()
	if err != nil {
		return nil, err
	}

	gzipCompression := cmdutil.IsTruthy(os.Getenv(PulumiFilestateGzipEnvVar))

	backend := &localBackend{
		d:              d,
		originalURL:    originalURL,
		url:            u,
		bucket:         b,
		lockID:         lockID.String(),
		gzip:           gzipCompression,
		currentProject: project,
	}

	projectMode := true
	switch v := pulumiState.Version; v {
	case 0:
		backend.store = &legacyReferenceStore{b: backend}
		projectMode = false
	case 1:
		backend.store = &projectReferenceStore{b: backend}
	default:
		return nil, fmt.Errorf(
			"state store unsupported: 'Pulumi.yaml' version (%d) is not supported "+
				"by this version of the Pulumi CLI", v)
	}

	if !projectMode {
		return backend, nil
	}

	// If we're in project mode warn about any old stack files.
	refs, err := (&legacyReferenceStore{b: backend}).ListReferences()
	if err != nil {
		// If there's an error listing don't fail, just don't print the warnings
		return backend, nil
	}

	for _, ref := range refs {
		d.Warningf(&diag.Diag{
			Message: "Found legacy stack file '%s', you should run 'pulumi state upgrade'",
		}, ref.Name())
	}

	return backend, nil
}

func (b *localBackend) Upgrade(ctx context.Context) error {
	// We don't use the existing b.store because
	// this may already be a projectReferenceStore
	// with new legacy files introduced to it accidentally.
	olds, err := (&legacyReferenceStore{b: b}).ListReferences()
	if err != nil {
		return fmt.Errorf("read old references: %w", err)
	}

	newStore := &projectReferenceStore{b: b}
	for _, old := range olds {
		chk, err := b.getCheckpoint(old)
		if err != nil {
			return err
		}
		// Try and find the project name from _any_ resource URN
		var project tokens.Name
		if chk.Latest != nil {
			for _, res := range chk.Latest.Resources {
				project = tokens.Name(res.URN.Project())
				break
			}
		}
		if project == "" {
			return fmt.Errorf("no project found for stack %v", old)
		}

		new := newStore.newReference(project, old.Name())
		if err := b.renameStack(ctx, old, new); err != nil {
			return fmt.Errorf("upgrade stack %v to %v: %w", old, new, err)
		}
	}

	pulumiYaml, err := yaml.Marshal(&pulumiMeta{Version: 1})
	contract.AssertNoErrorf(err, "Could not marshal filestate.pulumiState to yaml")
	if err = b.bucket.WriteAll(ctx, "Pulumi.yaml", pulumiYaml, nil); err != nil {
		return fmt.Errorf("could not write 'Pulumi.yaml': %w", err)
	}
	b.store = newStore

	return nil
}

// massageBlobPath takes the path the user provided and converts it to an appropriate form go-cloud
// can support.  Importantly, s3/azblob/gs paths should not be be touched. This will only affect
// file:// paths which have a few oddities around them that we want to ensure work properly.
func massageBlobPath(path string) (string, error) {
	if !strings.HasPrefix(path, FilePathPrefix) {
		// Not a file:// path.  Keep this untouched and pass directly to gocloud.
		return path, nil
	}

	// Strip off the "file://" portion so we can examine and determine what to do with the rest.
	path = strings.TrimPrefix(path, FilePathPrefix)

	// We need to specially handle ~.  The shell doesn't take care of this for us, and later
	// functions we run into can't handle this either.
	//
	// From https://stackoverflow.com/questions/17609732/expand-tilde-to-home-directory
	if strings.HasPrefix(path, "~") {
		usr, err := user.Current()
		if err != nil {
			return "", fmt.Errorf("Could not determine current user to resolve `file://~` path.: %w", err)
		}

		if path == "~" {
			path = usr.HomeDir
		} else {
			path = filepath.Join(usr.HomeDir, path[2:])
		}
	}

	// For file:// backend, ensure a relative path is resolved. fileblob only supports absolute paths.
	path, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("An IO error occurred while building the absolute path: %w", err)
	}

	// Using example from https://godoc.org/gocloud.dev/blob/fileblob#example-package--OpenBucket
	// On Windows, convert "\" to "/" and add a leading "/". (See https://gocloud.dev/howto/blob/#local)
	path = filepath.ToSlash(path)
	if os.PathSeparator != '/' && !strings.HasPrefix(path, "/") {
		path = "/" + path
	}

	return FilePathPrefix + path, nil
}

func Login(ctx context.Context, d diag.Sink, url string, project *workspace.Project) (Backend, error) {
	be, err := New(ctx, d, url, project)
	if err != nil {
		return nil, err
	}
	return be, workspace.StoreAccount(be.URL(), workspace.Account{}, true)
}

func (b *localBackend) getReference(ref backend.StackReference) (*localBackendReference, error) {
	stackRef, ok := ref.(*localBackendReference)
	if !ok {
		return nil, fmt.Errorf("bad stack reference type")
	}
	return stackRef, stackRef.Validate()
}

func (b *localBackend) local() {}

func (b *localBackend) Name() string {
	name, err := os.Hostname()
	contract.IgnoreError(err)
	if name == "" {
		name = "local"
	}
	return name
}

func (b *localBackend) URL() string {
	return b.originalURL
}

func (b *localBackend) StateDir() string {
	return workspace.BookkeepingDir
}

func (b *localBackend) SetCurrentProject(project *workspace.Project) {
	b.currentProject = project
}

func (b *localBackend) GetPolicyPack(ctx context.Context, policyPack string,
	d diag.Sink,
) (backend.PolicyPack, error) {
	return nil, fmt.Errorf("File state backend does not support resource policy")
}

func (b *localBackend) ListPolicyGroups(ctx context.Context, orgName string, _ backend.ContinuationToken) (
	apitype.ListPolicyGroupsResponse, backend.ContinuationToken, error,
) {
	return apitype.ListPolicyGroupsResponse{}, nil, fmt.Errorf("File state backend does not support resource policy")
}

func (b *localBackend) ListPolicyPacks(ctx context.Context, orgName string, _ backend.ContinuationToken) (
	apitype.ListPolicyPacksResponse, backend.ContinuationToken, error,
) {
	return apitype.ListPolicyPacksResponse{}, nil, fmt.Errorf("File state backend does not support resource policy")
}

func (b *localBackend) SupportsTags() bool {
	return false
}

func (b *localBackend) SupportsOrganizations() bool {
	return false
}

func (b *localBackend) ParseStackReference(stackRef string) (backend.StackReference, error) {
	return b.parseStackReference(stackRef)
}

func (b *localBackend) parseStackReference(stackRef string) (*localBackendReference, error) {
	return b.store.ParseReference(stackRef)
}

// ValidateStackName verifies the stack name is valid for the local backend.
func (b *localBackend) ValidateStackName(stackRef string) error {
	_, err := b.ParseStackReference(stackRef)
	return err
}

func (b *localBackend) DoesProjectExist(ctx context.Context, projectName string) (bool, error) {
	projects, err := b.getLocalProjects()
	if err != nil {
		return false, err
	}

	for _, project := range projects {
		if string(project) == projectName {
			return true, nil
		}
	}

	return false, nil
}

// Confirm the specified stack's project doesn't contradict the Pulumi.yaml of the current project. If the CWD
// is not in a Pulumi project, does not contradict. If the project name in Pulumi.yaml is "foo", a stack with a
// name of bar/foo should not work.
func currentProjectContradictsWorkspace(stack *localBackendReference) bool {
	contract.Requiref(stack != nil, "stack", "is nil")

	if stack.project == "" {
		return false
	}

	projPath, err := workspace.DetectProjectPath()
	if err != nil {
		return false
	}

	if projPath == "" {
		return false
	}

	proj, err := workspace.LoadProject(projPath)
	if err != nil {
		return false
	}

	return proj.Name.String() != stack.project.String()
}

func (b *localBackend) CreateStack(ctx context.Context, stackRef backend.StackReference,
	root string, opts interface{},
) (backend.Stack, error) {
	localStackRef, err := b.getReference(stackRef)
	if err != nil {
		return nil, err
	}

	err = b.Lock(ctx, stackRef)
	if err != nil {
		return nil, err
	}
	defer b.Unlock(ctx, stackRef)

	if currentProjectContradictsWorkspace(localStackRef) {
		return nil, fmt.Errorf("provided project name %q doesn't match Pulumi.yaml", localStackRef.project)
	}

	contract.Requiref(opts == nil, "opts", "local stacks do not support any options")

	stackName := localStackRef.FullyQualifiedName()
	if stackName == "" {
		return nil, errors.New("invalid empty stack name")
	}

	if _, _, err := b.getStack(ctx, localStackRef); err == nil {
		return nil, &backend.StackAlreadyExistsError{StackName: string(stackName)}
	}

	tags := backend.GetEnvironmentTagsForCurrentStack(root, b.currentProject)

	if err = validation.ValidateStackProperties(stackName.Name().String(), tags); err != nil {
		return nil, fmt.Errorf("validating stack properties: %w", err)
	}

	file, err := b.saveStack(localStackRef, nil, nil)
	if err != nil {
		return nil, err
	}

	stack := newStack(localStackRef, file, nil, b)
	b.d.Infof(diag.Message("", "Created stack '%s'"), stack.Ref())

	return stack, nil
}

func (b *localBackend) GetStack(ctx context.Context, stackRef backend.StackReference) (backend.Stack, error) {
	localStackRef, err := b.getReference(stackRef)
	if err != nil {
		return nil, err
	}

	snapshot, path, err := b.getStack(ctx, localStackRef)

	switch {
	case gcerrors.Code(err) == gcerrors.NotFound:
		return nil, nil
	case err != nil:
		return nil, err
	default:
		return newStack(localStackRef, path, snapshot, b), nil
	}
}

func (b *localBackend) ListStacks(
	ctx context.Context, _ backend.ListStacksFilter, _ backend.ContinuationToken) (
	[]backend.StackSummary, backend.ContinuationToken, error,
) {
	stacks, err := b.getLocalStacks()
	if err != nil {
		return nil, nil, err
	}

	// Note that the provided stack filter is not honored, since fields like
	// organizations and tags aren't persisted in the local backend.
	results := make([]backend.StackSummary, 0, len(stacks))
	for _, stackRef := range stacks {
		chk, err := b.getCheckpoint(stackRef)
		if err != nil {
			return nil, nil, err
		}
		results = append(results, newLocalStackSummary(stackRef, chk))
	}

	return results, nil, nil
}

func (b *localBackend) RemoveStack(ctx context.Context, stack backend.Stack, force bool) (bool, error) {
	localStackRef, err := b.getReference(stack.Ref())
	if err != nil {
		return false, err
	}

	err = b.Lock(ctx, localStackRef)
	if err != nil {
		return false, err
	}
	defer b.Unlock(ctx, localStackRef)

	snapshot, _, err := b.getStack(ctx, localStackRef)
	if err != nil {
		return false, err
	}

	// Don't remove stacks that still have resources.
	if !force && snapshot != nil && len(snapshot.Resources) > 0 {
		return true, errors.New("refusing to remove stack because it still contains resources")
	}

	return false, b.removeStack(localStackRef)
}

func (b *localBackend) RenameStack(ctx context.Context, stack backend.Stack,
	newName tokens.QName,
) (backend.StackReference, error) {
	localStackRef, err := b.getReference(stack.Ref())
	if err != nil {
		return nil, err
	}

	// Ensure the new stack name is valid.
	newRef, err := b.parseStackReference(string(newName))
	if err != nil {
		return nil, err
	}

	err = b.renameStack(ctx, localStackRef, newRef)
	if err != nil {
		return nil, err
	}

	return newRef, nil
}

func (b *localBackend) renameStack(ctx context.Context, oldRef *localBackendReference,
	newRef *localBackendReference,
) error {
	err := b.Lock(ctx, oldRef)
	if err != nil {
		return err
	}
	defer b.Unlock(ctx, oldRef)

	// Get the current state from the stack to be renamed.
	snap, _, err := b.getStack(ctx, oldRef)
	if err != nil {
		return err
	}

	// Ensure the destination stack does not already exist.
	hasExisting, err := b.bucket.Exists(ctx, b.stackPath(newRef))
	if err != nil {
		return err
	}
	if hasExisting {
		return fmt.Errorf("a stack named %s already exists", newRef.String())
	}

	// If we have a snapshot, we need to rename the URNs inside it to use the new stack name.
	if snap != nil {
		if err = edit.RenameStack(snap, newRef.name, ""); err != nil {
			return err
		}
	}

	// Now save the snapshot with a new name (we pass nil to re-use the existing secrets manager from the snapshot).
	if _, err = b.saveStack(newRef, snap, nil); err != nil {
		return err
	}

	// To remove the old stack, just make a backup of the file and don't write out anything new.
	file := b.stackPath(oldRef)
	backupTarget(b.bucket, file, false)

	// And rename the history folder as well.
	if err = b.renameHistory(oldRef, newRef); err != nil {
		return err
	}
	return err
}

func (b *localBackend) GetLatestConfiguration(ctx context.Context,
	stack backend.Stack,
) (config.Map, error) {
	hist, err := b.GetHistory(ctx, stack.Ref(), 1 /*pageSize*/, 1 /*page*/)
	if err != nil {
		return nil, err
	}
	if len(hist) == 0 {
		return nil, backend.ErrNoPreviousDeployment
	}

	return hist[0].Config, nil
}

func (b *localBackend) PackPolicies(
	ctx context.Context, policyPackRef backend.PolicyPackReference,
	cancellationScopes backend.CancellationScopeSource,
	callerEventsOpt chan<- engine.Event,
) result.Result {
	return result.Error("File state backend does not support resource policy")
}

func (b *localBackend) Preview(ctx context.Context, stack backend.Stack,
	op backend.UpdateOperation,
) (*deploy.Plan, sdkDisplay.ResourceChanges, result.Result) {
	// We can skip PreviewThenPromptThenExecute and just go straight to Execute.
	opts := backend.ApplierOptions{
		DryRun:   true,
		ShowLink: true,
	}
	return b.apply(ctx, apitype.PreviewUpdate, stack, op, opts, nil /*events*/)
}

func (b *localBackend) Update(ctx context.Context, stack backend.Stack,
	op backend.UpdateOperation,
) (sdkDisplay.ResourceChanges, result.Result) {
	err := b.Lock(ctx, stack.Ref())
	if err != nil {
		return nil, result.FromError(err)
	}
	defer b.Unlock(ctx, stack.Ref())

	return backend.PreviewThenPromptThenExecute(ctx, apitype.UpdateUpdate, stack, op, b.apply)
}

func (b *localBackend) Import(ctx context.Context, stack backend.Stack,
	op backend.UpdateOperation, imports []deploy.Import,
) (sdkDisplay.ResourceChanges, result.Result) {
	err := b.Lock(ctx, stack.Ref())
	if err != nil {
		return nil, result.FromError(err)
	}
	defer b.Unlock(ctx, stack.Ref())

	op.Imports = imports
	return backend.PreviewThenPromptThenExecute(ctx, apitype.ResourceImportUpdate, stack, op, b.apply)
}

func (b *localBackend) Refresh(ctx context.Context, stack backend.Stack,
	op backend.UpdateOperation,
) (sdkDisplay.ResourceChanges, result.Result) {
	err := b.Lock(ctx, stack.Ref())
	if err != nil {
		return nil, result.FromError(err)
	}
	defer b.Unlock(ctx, stack.Ref())

	return backend.PreviewThenPromptThenExecute(ctx, apitype.RefreshUpdate, stack, op, b.apply)
}

func (b *localBackend) Destroy(ctx context.Context, stack backend.Stack,
	op backend.UpdateOperation,
) (sdkDisplay.ResourceChanges, result.Result) {
	err := b.Lock(ctx, stack.Ref())
	if err != nil {
		return nil, result.FromError(err)
	}
	defer b.Unlock(ctx, stack.Ref())

	return backend.PreviewThenPromptThenExecute(ctx, apitype.DestroyUpdate, stack, op, b.apply)
}

func (b *localBackend) Query(ctx context.Context, op backend.QueryOperation) result.Result {
	return b.query(ctx, op, nil /*events*/)
}

func (b *localBackend) Watch(ctx context.Context, stk backend.Stack,
	op backend.UpdateOperation, paths []string,
) result.Result {
	return backend.Watch(ctx, stack.DefaultSecretsProvider, b, stk, op, b.apply, paths)
}

// apply actually performs the provided type of update on a locally hosted stack.
func (b *localBackend) apply(
	ctx context.Context, kind apitype.UpdateKind, stack backend.Stack,
	op backend.UpdateOperation, opts backend.ApplierOptions,
	events chan<- engine.Event,
) (*deploy.Plan, sdkDisplay.ResourceChanges, result.Result) {
	stackRef := stack.Ref()
	localStackRef, err := b.getReference(stackRef)
	if err != nil {
		return nil, nil, result.FromError(err)
	}

	if currentProjectContradictsWorkspace(localStackRef) {
		return nil, nil, result.Errorf("provided project name %q doesn't match Pulumi.yaml", localStackRef.project)
	}

	stackName := stackRef.FullyQualifiedName()
	actionLabel := backend.ActionLabel(kind, opts.DryRun)

	if !(op.Opts.Display.JSONDisplay || op.Opts.Display.Type == display.DisplayWatch) {
		// Print a banner so it's clear this is a local deployment.
		fmt.Printf(op.Opts.Display.Color.Colorize(
			colors.SpecHeadline+"%s (%s):"+colors.Reset+"\n"), actionLabel, stackRef)
	}

	// Start the update.
	update, err := b.newUpdate(ctx, localStackRef, op)
	if err != nil {
		return nil, nil, result.FromError(err)
	}

	// Spawn a display loop to show events on the CLI.
	displayEvents := make(chan engine.Event)
	displayDone := make(chan bool)
	go display.ShowEvents(
		strings.ToLower(actionLabel), kind, stackName.Name(), op.Proj.Name, "",
		displayEvents, displayDone, op.Opts.Display, opts.DryRun)

	// Create a separate event channel for engine events that we'll pipe to both listening streams.
	engineEvents := make(chan engine.Event)

	scope := op.Scopes.NewScope(engineEvents, opts.DryRun)
	eventsDone := make(chan bool)
	go func() {
		// Pull in all events from the engine and send them to the two listeners.
		for e := range engineEvents {
			displayEvents <- e

			// If the caller also wants to see the events, stream them there also.
			if events != nil {
				events <- e
			}
		}

		close(eventsDone)
	}()

	// Create the management machinery.
	persister := b.newSnapshotPersister(localStackRef, op.SecretsManager)
	manager := backend.NewSnapshotManager(persister, update.GetTarget().Snapshot)
	engineCtx := &engine.Context{
		Cancel:          scope.Context(),
		Events:          engineEvents,
		SnapshotManager: manager,
		BackendClient:   backend.NewBackendClient(b, op.SecretsProvider),
	}

	// Perform the update
	start := time.Now().Unix()
	var plan *deploy.Plan
	var changes sdkDisplay.ResourceChanges
	var updateRes result.Result
	switch kind {
	case apitype.PreviewUpdate:
		plan, changes, updateRes = engine.Update(update, engineCtx, op.Opts.Engine, true)
	case apitype.UpdateUpdate:
		_, changes, updateRes = engine.Update(update, engineCtx, op.Opts.Engine, opts.DryRun)
	case apitype.ResourceImportUpdate:
		_, changes, updateRes = engine.Import(update, engineCtx, op.Opts.Engine, op.Imports, opts.DryRun)
	case apitype.RefreshUpdate:
		_, changes, updateRes = engine.Refresh(update, engineCtx, op.Opts.Engine, opts.DryRun)
	case apitype.DestroyUpdate:
		_, changes, updateRes = engine.Destroy(update, engineCtx, op.Opts.Engine, opts.DryRun)
	default:
		contract.Failf("Unrecognized update kind: %s", kind)
	}
	end := time.Now().Unix()

	// Wait for the display to finish showing all the events.
	<-displayDone
	scope.Close() // Don't take any cancellations anymore, we're shutting down.
	close(engineEvents)
	contract.IgnoreClose(manager)

	// Make sure the goroutine writing to displayEvents and events has exited before proceeding.
	<-eventsDone
	close(displayEvents)

	// Save update results.
	backendUpdateResult := backend.SucceededResult
	if updateRes != nil {
		backendUpdateResult = backend.FailedResult
	}
	info := backend.UpdateInfo{
		Kind:        kind,
		StartTime:   start,
		Message:     op.M.Message,
		Environment: op.M.Environment,
		Config:      update.GetTarget().Config,
		Result:      backendUpdateResult,
		EndTime:     end,
		// IDEA: it would be nice to populate the *Deployment, so that addToHistory below doesn't need to
		//     rudely assume it knows where the checkpoint file is on disk as it makes a copy of it.  This isn't
		//     trivial to achieve today given the event driven nature of plan-walking, however.
		ResourceChanges: changes,
	}

	var saveErr error
	var backupErr error
	if !opts.DryRun {
		saveErr = b.addToHistory(localStackRef, info)
		backupErr = b.backupStack(localStackRef)
	}

	if updateRes != nil {
		// We swallow saveErr and backupErr as they are less important than the updateErr.
		return plan, changes, updateRes
	}

	if saveErr != nil {
		// We swallow backupErr as it is less important than the saveErr.
		return plan, changes, result.FromError(fmt.Errorf("saving update info: %w", saveErr))
	}

	if backupErr != nil {
		return plan, changes, result.FromError(fmt.Errorf("saving backup: %w", backupErr))
	}

	// Make sure to print a link to the stack's checkpoint before exiting.
	if !op.Opts.Display.SuppressPermalink && opts.ShowLink && !op.Opts.Display.JSONDisplay {
		// Note we get a real signed link for aws/azure/gcp links.  But no such option exists for
		// file:// links so we manually create the link ourselves.
		var link string
		if strings.HasPrefix(b.url, FilePathPrefix) {
			u, _ := url.Parse(b.url)
			u.Path = filepath.ToSlash(path.Join(u.Path, b.stackPath(localStackRef)))
			link = u.String()
		} else {
			link, err = b.bucket.SignedURL(ctx, b.stackPath(localStackRef), nil)
			if err != nil {
				// set link to be empty to when there is an error to hide use of Permalinks
				link = ""

				// we log a warning here rather then returning an error to avoid exiting
				// pulumi with an error code.
				// printing a statefile perma link happens after all the providers have finished
				// deploying the infrastructure, failing the pulumi update because there was a
				// problem printing a statefile perma link can be missleading in automated CI environments.
				cmdutil.Diag().Warningf(diag.Message("", "Unable to create signed url for current backend to "+
					"create a Permalink. Please visit https://www.pulumi.com/docs/troubleshooting/ "+
					"for more information\n"))
			}
		}

		if link != "" {
			fmt.Printf(op.Opts.Display.Color.Colorize(
				colors.SpecHeadline+"Permalink: "+
					colors.Underline+colors.BrightBlue+"%s"+colors.Reset+"\n"), link)
		}
	}

	return plan, changes, nil
}

// query executes a query program against the resource outputs of a locally hosted stack.
func (b *localBackend) query(ctx context.Context, op backend.QueryOperation,
	callerEventsOpt chan<- engine.Event,
) result.Result {
	return backend.RunQuery(ctx, b, op, callerEventsOpt, b.newQuery)
}

func (b *localBackend) GetHistory(
	ctx context.Context,
	stackRef backend.StackReference,
	pageSize int,
	page int,
) ([]backend.UpdateInfo, error) {
	localStackRef, err := b.getReference(stackRef)
	if err != nil {
		return nil, err
	}
	updates, err := b.getHistory(localStackRef, pageSize, page)
	if err != nil {
		return nil, err
	}
	return updates, nil
}

func (b *localBackend) GetLogs(ctx context.Context,
	secretsProvider secrets.Provider, stack backend.Stack, cfg backend.StackConfiguration,
	query operations.LogQuery,
) ([]operations.LogEntry, error) {
	localStackRef, err := b.getReference(stack.Ref())
	if err != nil {
		return nil, err
	}

	target, err := b.getTarget(ctx, localStackRef, cfg.Config, cfg.Decrypter)
	if err != nil {
		return nil, err
	}

	return GetLogsForTarget(target, query)
}

// GetLogsForTarget fetches stack logs using the config, decrypter, and checkpoint in the given target.
func GetLogsForTarget(target *deploy.Target, query operations.LogQuery) ([]operations.LogEntry, error) {
	contract.Requiref(target != nil, "target", "must not be nil")

	if target.Snapshot == nil {
		// If the stack has not been deployed yet, return no logs.
		return nil, nil
	}

	config, err := target.Config.Decrypt(target.Decrypter)
	if err != nil {
		return nil, err
	}

	components := operations.NewResourceTree(target.Snapshot.Resources)
	ops := components.OperationsProvider(config)
	logs, err := ops.GetLogs(query)
	if logs == nil {
		return nil, err
	}
	return *logs, err
}

func (b *localBackend) ExportDeployment(ctx context.Context,
	stk backend.Stack,
) (*apitype.UntypedDeployment, error) {
	localStackRef, err := b.getReference(stk.Ref())
	if err != nil {
		return nil, err
	}

	chk, err := b.getCheckpoint(localStackRef)
	if err != nil {
		return nil, fmt.Errorf("failed to load checkpoint: %w", err)
	}

	data, err := encoding.JSON.Marshal(chk.Latest)
	if err != nil {
		return nil, err
	}

	return &apitype.UntypedDeployment{
		Version:    3,
		Deployment: json.RawMessage(data),
	}, nil
}

func (b *localBackend) ImportDeployment(ctx context.Context, stk backend.Stack,
	deployment *apitype.UntypedDeployment,
) error {
	localStackRef, err := b.getReference(stk.Ref())
	if err != nil {
		return err
	}

	err = b.Lock(ctx, localStackRef)
	if err != nil {
		return err
	}
	defer b.Unlock(ctx, localStackRef)

	stackName := localStackRef.FullyQualifiedName()
	chk, err := stack.MarshalUntypedDeploymentToVersionedCheckpoint(stackName, deployment)
	if err != nil {
		return err
	}

	_, _, err = b.saveCheckpoint(localStackRef, chk)
	return err
}

func (b *localBackend) Logout() error {
	return workspace.DeleteAccount(b.originalURL)
}

func (b *localBackend) LogoutAll() error {
	return workspace.DeleteAllAccounts()
}

func (b *localBackend) CurrentUser() (string, []string, error) {
	user, err := user.Current()
	if err != nil {
		return "", nil, err
	}
	return user.Username, nil, nil
}

func (b *localBackend) getLocalStacks() ([]*localBackendReference, error) {
	return b.store.ListReferences()
}

func (b *localBackend) getLocalProjects() ([]tokens.Name, error) {
	files, err := listBucket(b.bucket, StacksDir)
	if err != nil {
		return nil, fmt.Errorf("error listing projects: %w", err)
	}
	projects := make([]tokens.Name, 0, len(files))

	for _, file := range files {
		// Ignore files.
		if !file.IsDir {
			continue
		}

		// Skip directories without valid names
		objName := objectName(file)
		if !tokens.IsName(objName) {
			continue
		}

		projects = append(projects, tokens.Name(objName))
	}

	return projects, nil
}

// GetStackTags fetches the stack's existing tags.
func (b *localBackend) GetStackTags(ctx context.Context,
	stack backend.Stack,
) (map[apitype.StackTagName]string, error) {
	// The local backend does not currently persist tags.
	return nil, errors.New("stack tags not supported in --local mode")
}

// UpdateStackTags updates the stacks's tags, replacing all existing tags.
func (b *localBackend) UpdateStackTags(ctx context.Context,
	stack backend.Stack, tags map[apitype.StackTagName]string,
) error {
	// The local backend does not currently persist tags.
	return errors.New("stack tags not supported in --local mode")
}

func (b *localBackend) CancelCurrentUpdate(ctx context.Context, stackRef backend.StackReference) error {
	// Try to delete ALL the lock files
	allFiles, err := listBucket(b.bucket, stackLockDir(stackRef.FullyQualifiedName()))
	if err != nil {
		// Don't error if it just wasn't found
		if gcerrors.Code(err) == gcerrors.NotFound {
			return nil
		}
		return err
	}

	for _, file := range allFiles {
		if file.IsDir {
			continue
		}

		err := b.bucket.Delete(ctx, file.Key)
		if err != nil {
			// Race condition, don't error if the file was delete between us calling list and now
			if gcerrors.Code(err) == gcerrors.NotFound {
				return nil
			}
			return err
		}
	}

	return nil
}
