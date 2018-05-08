// Copyright 2011 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package buildenv contains definitions for the
// environments the Go build system can run in.
package buildenv

import (
	"context"
	"flag"
	"fmt"
	"io/ioutil"
	"log"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/oauth2/google"
	compute "google.golang.org/api/compute/v1"
	oauth2api "google.golang.org/api/oauth2/v2"
)

const (
	prefix = "https://www.googleapis.com/compute/v1/projects/"
)

// KubeConfig describes the configuration of a Kubernetes cluster.
type KubeConfig struct {
	// MinNodes is the minimum number of nodes in the Kubernetes cluster.
	// The autoscaler will ensure that at least this many nodes is always
	// running despite any scale-down decision.
	MinNodes int64

	// MaxNodes is the maximum number of nodes that the autoscaler can
	// provision in the Kubernetes cluster.
	// If MaxNodes is 0, Kubernetes is not used.
	MaxNodes int64

	// MachineType is the GCE machine type to use for the Kubernetes cluster nodes.
	MachineType string

	// Name is the name of the Kubernetes cluster that will be created.
	Name string
}

// Environment describes the configuration of the infrastructure for a
// coordinator and its buildlet resources running on Google Cloud Platform.
// Staging and Production are the two common build environments.
type Environment struct {
	// The GCP project name that the build infrastructure will be provisioned in.
	// This field may be overridden as necessary without impacting other fields.
	ProjectName string

	// ProjectNumber is the GCP project's number, as visible in the admin console.
	// This is used for things such as constructing the "email" of the default
	// service account.
	ProjectNumber int64

	// The IsProd flag indicates whether production functionality should be
	// enabled. When true, GCE and Kubernetes builders are enabled and the
	// coordinator serves on 443. Otherwise, GCE and Kubernetes builders are
	// disabled and the coordinator serves on 8119.
	IsProd bool

	// Zone is the GCE zone that the coordinator instance and Kubernetes cluster
	// will run in. This field may be overridden as necessary without impacting
	// other fields.
	Zone string

	// ZonesToClean are the GCE zones that will be periodically cleaned by
	// deleting old VMs. The zero value means that no cleaning will occur.
	// This field is optional.
	ZonesToClean []string

	// StaticIP is the public, static IP address that will be attached to the
	// coordinator instance. The zero value means the address will be looked
	// up by name. This field is optional.
	StaticIP string

	// MachineType is the GCE machine type to use for the coordinator.
	MachineType string

	// KubeBuild is the Kubernetes config for the buildlet cluster.
	KubeBuild KubeConfig
	// KubeTools is the Kubernetes config for the tools cluster.
	KubeTools KubeConfig

	// DashURL is the base URL of the build dashboard, ending in a slash.
	DashURL string

	// PerfDataURL is the base URL of the benchmark storage server.
	PerfDataURL string

	// CoordinatorURL is the location from which the coordinator
	// binary will be downloaded.
	// This is only used by cmd/coordinator/buildongce/create.go when
	// creating the coordinator VM from scratch.
	CoordinatorURL string

	// CoordinatorName is the hostname of the coordinator instance.
	CoordinatorName string

	// BuildletBucket is the GCS bucket that stores buildlet binaries.
	// TODO: rename. this is not just for buildlets; also for bootstrap.
	BuildletBucket string

	// LogBucket is the GCS bucket to which logs are written.
	LogBucket string

	// SnapBucket is the GCS bucket to which snapshots of
	// completed builds (after make.bash, before tests) are
	// written.
	SnapBucket string

	// MaxBuilds is the maximum number of concurrent builds that
	// can run. Zero means unlimit. This is typically only used
	// in a development or staging environment.
	MaxBuilds int

	// AutoCertCacheBucket is the GCS bucket to use for the
	// golang.org/x/crypto/acme/autocert (LetsEncrypt) cache.
	// If empty, LetsEncrypt isn't used.
	AutoCertCacheBucket string
}

// MachineTypeURI returns the URI for the environment's Machine Type.
func (e Environment) MachineTypeURI() string {
	return e.ComputePrefix() + "/zones/" + e.Zone + "/machineTypes/" + e.MachineType
}

// ComputePrefix returns the URI prefix for Compute Engine resources in a project.
func (e Environment) ComputePrefix() string {
	return prefix + e.ProjectName
}

// Region returns the GCE region, derived from its zone.
func (e Environment) Region() string {
	return e.Zone[:strings.LastIndex(e.Zone, "-")]
}

// SnapshotURL returns the absolute URL of the .tar.gz containing a
// built Go tree for the builderType and Go rev (40 character Git
// commit hash). The tarball is suitable for passing to
// (*buildlet.Client).PutTarFromURL.
func (e Environment) SnapshotURL(builderType, rev string) string {
	return fmt.Sprintf("https://storage.googleapis.com/%s/go/%s/%s.tar.gz", e.SnapBucket, builderType, rev)
}

// DashBase returns the base URL of the build dashboard, ending in a slash.
func (e Environment) DashBase() string {
	// TODO(quentin): Should we really default to production? That's what the old code did.
	if e.DashURL != "" {
		return e.DashURL
	}
	return Production.DashURL
}

// Credentials returns the credentials required to access the GCP environment.
//
// It tries to use, in order:
//
//   - the file $HOME/keys/$PROJECT_NAME.key.json
//   - the file $HOME/.config/gcloud/legacy_credentials/$ANYTHING@google.com/adc.json
//   - the Application Default Credentials (i.e. GCE metadata service, etc)
func (e Environment) Credentials(ctx context.Context) (*google.Credentials, error) {
	scopes := []string{
		// Cloud Platform should include all others, but the
		// old code duplicated compute and the storage full
		// control scopes, so I leave them here for now. They
		// predated the all-encompassing "cloud platform"
		// scope anyway.
		// TODO: remove compute and DevstorageFullControlScope once verified to work
		// without.
		compute.CloudPlatformScope,
		compute.ComputeScope,
		compute.DevstorageFullControlScope,

		// The coordinator needed the userinfo email scope for
		// reporting to the perf dashboard running on App
		// Engine at one point. The perf dashboard is down at
		// the moment, but when it's back up we'll need this,
		// and if we do other authenticated requests to App
		// Engine apps, this would be useful.
		oauth2api.UserinfoEmailScope,
	}

	// Prefer any "$HOME/keys/$PROJECT.key.json" file first.
	keyFile := filepath.Join(os.Getenv("HOME"), "keys", e.ProjectName+".key.json")
	if _, err := os.Stat(keyFile); err == nil {
		log.Printf("Using credentials from %s", keyFile)
		jcred, err := ioutil.ReadFile(keyFile)
		if err != nil {
			return nil, err
		}
		return google.CredentialsFromJSON(ctx, jcred, scopes...)
	}

	// Then prefer a gcloud @google.com user.
	if n := findGoogUserJSON(); n != "" {
		jsonData, err := ioutil.ReadFile(n)
		if err != nil {
			return nil, err
		}
		creds, err := google.CredentialsFromJSON(ctx, jsonData, scopes...)
		if err != nil {
			log.Printf("gobuild.NewClient: error loading google user credentials from %s: %v", n, err)
			return nil, err
		}
		log.Printf("Using credentials from %s", n)
		return creds, nil
	}

	creds, err := google.FindDefaultCredentials(ctx, scopes...)
	if err != nil {
		return nil, err
	}
	log.Printf("Using Application Default Credentials")
	return creds, nil
}

// findGoogUserJSON walks the gcloud config directory and returns the filename of
// $HOME/.config/gcloud/legacy_credentials/USER@google.com/adc.json
// for the first USER found with such a file. On miss it returns the empty string.
func findGoogUserJSON() (jsonFile string) {
	gcloud := filepath.Join(os.Getenv("HOME"), ".config/gcloud")
	filepath.Walk(gcloud, func(path string, fi os.FileInfo, err error) error {
		if jsonFile == "" && err == nil && fi.Mode().IsRegular() && strings.HasSuffix(path, "@google.com/adc.json") &&
			strings.HasPrefix(path, filepath.Join(gcloud, "legacy_credentials")+"/") {
			jsonFile = path
		}
		return nil
	})
	return
}

// ByProjectID returns an Environment for the specified
// project ID. It is currently limited to the symbolic-datum-552
// and go-dashboard-dev projects.
// ByProjectID will panic if the project ID is not known.
func ByProjectID(projectID string) *Environment {
	var envKeys []string

	for k := range possibleEnvs {
		envKeys = append(envKeys, k)
	}

	var env *Environment
	env, ok := possibleEnvs[projectID]
	if !ok {
		panic(fmt.Sprintf("Can't get buildenv for unknown project %q. Possible envs are %s", projectID, envKeys))
	}

	return env
}

// Staging defines the environment that the coordinator and build
// infrastructure is deployed to before it is released to production.
// For local dev, override the project with the program's flag to set
// a custom project.
var Staging = &Environment{
	ProjectName:   "go-dashboard-dev",
	ProjectNumber: 302018677728,
	IsProd:        true,
	Zone:          "us-central1-f",
	ZonesToClean:  []string{"us-central1-a", "us-central1-b", "us-central1-f"},
	StaticIP:      "104.154.113.235",
	MachineType:   "n1-standard-1",
	KubeBuild: KubeConfig{
		MinNodes:    1,
		MaxNodes:    2,
		Name:        "buildlets",
		MachineType: "n1-standard-8",
	},
	KubeTools: KubeConfig{
		MinNodes:    3,
		MaxNodes:    3,
		Name:        "go",
		MachineType: "n1-standard-4",
	},
	DashURL:         "https://go-dashboard-dev.appspot.com/",
	PerfDataURL:     "https://perfdata.golang.org",
	CoordinatorURL:  "https://storage.googleapis.com/dev-go-builder-data/coordinator",
	CoordinatorName: "farmer",
	BuildletBucket:  "dev-go-builder-data",
	LogBucket:       "dev-go-build-log",
	SnapBucket:      "dev-go-build-snap",
}

// Production defines the environment that the coordinator and build
// infrastructure is deployed to for production usage at build.golang.org.
var Production = &Environment{
	ProjectName:   "symbolic-datum-552",
	ProjectNumber: 872405196845,
	IsProd:        true,
	Zone:          "us-central1-f",
	ZonesToClean:  []string{"us-central1-f"},
	StaticIP:      "107.178.219.46",
	MachineType:   "n1-standard-4",
	KubeBuild: KubeConfig{
		MinNodes:    5,
		MaxNodes:    5, // auto-scaling disabled
		Name:        "buildlets",
		MachineType: "n1-standard-32",
	},
	KubeTools: KubeConfig{
		MinNodes:    3,
		MaxNodes:    3,
		Name:        "go",
		MachineType: "n1-standard-4",
	},
	DashURL:             "https://build.golang.org/",
	PerfDataURL:         "https://perfdata.golang.org",
	CoordinatorURL:      "https://storage.googleapis.com/go-builder-data/coordinator",
	CoordinatorName:     "farmer",
	BuildletBucket:      "go-builder-data",
	LogBucket:           "go-build-log",
	SnapBucket:          "go-build-snap",
	AutoCertCacheBucket: "farmer-golang-org-autocert-cache",
}

var Development = &Environment{
	IsProd:   false,
	StaticIP: "127.0.0.1",
}

// possibleEnvs enumerate the known buildenv.Environment definitions.
var possibleEnvs = map[string]*Environment{
	"dev":                Development,
	"symbolic-datum-552": Production,
	"go-dashboard-dev":   Staging,
}

var (
	stagingFlag     bool
	localDevFlag    bool
	registeredFlags bool
)

// RegisterFlags registers the "staging" and "localdev" flags.
func RegisterFlags() {
	if registeredFlags {
		panic("duplicate call to RegisterFlags or RegisterStagingFlag")
	}
	flag.BoolVar(&localDevFlag, "localdev", false, "use the localhost in-development coordinator")
	RegisterStagingFlag()
	registeredFlags = true
}

// RegisterStagingFlag registers the "staging" flag.
func RegisterStagingFlag() {
	if registeredFlags {
		panic("duplicate call to RegisterFlags or RegisterStagingFlag")
	}
	flag.BoolVar(&stagingFlag, "staging", false, "use the staging build coordinator and buildlets")
	registeredFlags = true
}

// FromFlags returns the build environment specified from flags,
// as registered by RegisterFlags or RegisterStagingFlag.
// By default it returns the production environment.
func FromFlags() *Environment {
	if !registeredFlags {
		panic("FromFlags called without RegisterFlags")
	}
	if localDevFlag {
		return Development
	}
	if stagingFlag {
		return Staging
	}
	return Production
}
