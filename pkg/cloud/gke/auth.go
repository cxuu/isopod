// Copyright 2019 GM Cruise LLC
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

package gke

import (
	"context"
	"encoding/base64"
	"fmt"
	"io/ioutil"

	"golang.org/x/oauth2"
	"golang.org/x/oauth2/google"
	"google.golang.org/api/container/v1"
	"google.golang.org/api/option"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/transport"
)

// GoogleCredTokenSourceFromSAKey creates a oauth2 token source from google service account key json.
func GoogleCredTokenSourceFromSAKey(ctx context.Context, svcAcctKeyFile string) (oauth2.TokenSource, error) {
	b, err := ioutil.ReadFile(svcAcctKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to read the SA key json file `%s': %v", svcAcctKeyFile, err)
	}
	cred, err := google.CredentialsFromJSON(ctx, b, container.CloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("failed to extract credentials from json: %v", err)
	}
	return cred.TokenSource, nil
}

// BuildKubeRestConfSACred creates a k8s rest.Config using service account JSON
// key file. If such key is empty, fall back to using default application cred.
func BuildKubeRestConfSACred(
	ctx context.Context,
	clusterName, location, project, svcAcctKeyFile, userAgent string,
) (*rest.Config, error) {
	if svcAcctKeyFile == "" {
		return BuildKubeRestConfDefaultCred(ctx, clusterName, location, project, userAgent)
	}
	TokenSrc, err := GoogleCredTokenSourceFromSAKey(ctx, svcAcctKeyFile)
	if err != nil {
		return nil, fmt.Errorf("failed to get token source from service account: %v", err)
	}
	return buildKubeRestConf(ctx, clusterName, location, project, userAgent, TokenSrc)
}

// BuildKubeRestConfDefaultCred creates a k8s rest.Config using the google
// application default credential.
func BuildKubeRestConfDefaultCred(
	ctx context.Context,
	clusterName, location, project, userAgent string,
) (*rest.Config, error) {
	tokenSrc, err := google.DefaultTokenSource(ctx, container.CloudPlatformScope)
	if err != nil {
		return nil, fmt.Errorf("failed to create the google DefaultTokenSource: %v", err)
	}
	return buildKubeRestConf(ctx, clusterName, location, project, userAgent, tokenSrc)
}

func buildKubeRestConf(
	ctx context.Context,
	clusterName, location, project, userAgent string,
	tokenSrc oauth2.TokenSource,
) (*rest.Config, error) {
	containerSvc, err := container.NewService(ctx, option.WithTokenSource(tokenSrc))
	if err != nil {
		return nil, fmt.Errorf("failed to create the container service: %v", err)
	}
	name := fmt.Sprintf("projects/%s/locations/%s/clusters/%s", project, location, clusterName)
	cluster, err := containerSvc.Projects.Locations.Clusters.Get(name).Context(ctx).Do()
	if err != nil {
		return nil, fmt.Errorf("failed to retrieve cluster info: %v", err)
	}

	// ClusterCaCertificate is pem encoded and then base64 encoded.
	caCert, err := base64.StdEncoding.DecodeString(cluster.MasterAuth.ClusterCaCertificate)
	if err != nil {
		return nil, fmt.Errorf("failed to base64 decode the cluster CA cert: %v", err)
	}
	return &rest.Config{
		Host: "https://" + cluster.Endpoint,
		TLSClientConfig: rest.TLSClientConfig{
			CAData: caCert, // pem encoded
		},
		WrapTransport: transport.TokenSourceWrapTransport(tokenSrc),
		UserAgent:     userAgent,
	}, nil
}
