package client

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/distribution/reference"
	"github.com/moby/moby/api/types/registry"
	"github.com/moby/moby/api/types/swarm"
	"github.com/moby/moby/api/types/versions"
	"github.com/opencontainers/go-digest"
	"github.com/pkg/errors"
)

// ServiceCreate creates a new service.
func (cli *Client) ServiceCreate(ctx context.Context, service swarm.ServiceSpec, options swarm.ServiceCreateOptions) (swarm.ServiceCreateResponse, error) {
	var response swarm.ServiceCreateResponse

	// Make sure we negotiated (if the client is configured to do so),
	// as code below contains API-version specific handling of options.
	//
	// Normally, version-negotiation (if enabled) would not happen until
	// the API request is made.
	if err := cli.checkVersion(ctx); err != nil {
		return response, err
	}

	// Make sure containerSpec is not nil when no runtime is set or the runtime is set to container
	if service.TaskTemplate.ContainerSpec == nil && (service.TaskTemplate.Runtime == "" || service.TaskTemplate.Runtime == swarm.RuntimeContainer) {
		service.TaskTemplate.ContainerSpec = &swarm.ContainerSpec{}
	}

	if err := validateServiceSpec(service); err != nil {
		return response, err
	}
	if versions.LessThan(cli.version, "1.30") {
		if err := validateAPIVersion(service, cli.version); err != nil {
			return response, err
		}
	}

	// ensure that the image is tagged
	var resolveWarning string
	switch {
	case service.TaskTemplate.ContainerSpec != nil:
		if taggedImg := imageWithTagString(service.TaskTemplate.ContainerSpec.Image); taggedImg != "" {
			service.TaskTemplate.ContainerSpec.Image = taggedImg
		}
		if options.QueryRegistry {
			resolveWarning = resolveContainerSpecImage(ctx, cli, &service.TaskTemplate, options.EncodedRegistryAuth)
		}
	case service.TaskTemplate.PluginSpec != nil:
		if taggedImg := imageWithTagString(service.TaskTemplate.PluginSpec.Remote); taggedImg != "" {
			service.TaskTemplate.PluginSpec.Remote = taggedImg
		}
		if options.QueryRegistry {
			resolveWarning = resolvePluginSpecRemote(ctx, cli, &service.TaskTemplate, options.EncodedRegistryAuth)
		}
	}

	headers := http.Header{}
	if versions.LessThan(cli.version, "1.30") {
		// the custom "version" header was used by engine API before 20.10
		// (API 1.30) to switch between client- and server-side lookup of
		// image digests.
		headers["version"] = []string{cli.version}
	}
	if options.EncodedRegistryAuth != "" {
		headers[registry.AuthHeader] = []string{options.EncodedRegistryAuth}
	}
	resp, err := cli.post(ctx, "/services/create", nil, service, headers)
	defer ensureReaderClosed(resp)
	if err != nil {
		return response, err
	}

	err = json.NewDecoder(resp.Body).Decode(&response)
	if resolveWarning != "" {
		response.Warnings = append(response.Warnings, resolveWarning)
	}

	return response, err
}

func resolveContainerSpecImage(ctx context.Context, cli DistributionAPIClient, taskSpec *swarm.TaskSpec, encodedAuth string) string {
	var warning string
	if img, imgPlatforms, err := imageDigestAndPlatforms(ctx, cli, taskSpec.ContainerSpec.Image, encodedAuth); err != nil {
		warning = digestWarning(taskSpec.ContainerSpec.Image)
	} else {
		taskSpec.ContainerSpec.Image = img
		if len(imgPlatforms) > 0 {
			if taskSpec.Placement == nil {
				taskSpec.Placement = &swarm.Placement{}
			}
			taskSpec.Placement.Platforms = imgPlatforms
		}
	}
	return warning
}

func resolvePluginSpecRemote(ctx context.Context, cli DistributionAPIClient, taskSpec *swarm.TaskSpec, encodedAuth string) string {
	var warning string
	if img, imgPlatforms, err := imageDigestAndPlatforms(ctx, cli, taskSpec.PluginSpec.Remote, encodedAuth); err != nil {
		warning = digestWarning(taskSpec.PluginSpec.Remote)
	} else {
		taskSpec.PluginSpec.Remote = img
		if len(imgPlatforms) > 0 {
			if taskSpec.Placement == nil {
				taskSpec.Placement = &swarm.Placement{}
			}
			taskSpec.Placement.Platforms = imgPlatforms
		}
	}
	return warning
}

func imageDigestAndPlatforms(ctx context.Context, cli DistributionAPIClient, image, encodedAuth string) (string, []swarm.Platform, error) {
	distributionInspect, err := cli.DistributionInspect(ctx, image, encodedAuth)
	var platforms []swarm.Platform
	if err != nil {
		return "", nil, err
	}

	imageWithDigest := imageWithDigestString(image, distributionInspect.Descriptor.Digest)

	if len(distributionInspect.Platforms) > 0 {
		platforms = make([]swarm.Platform, 0, len(distributionInspect.Platforms))
		for _, p := range distributionInspect.Platforms {
			// clear architecture field for arm. This is a temporary patch to address
			// https://github.com/docker/swarmkit/issues/2294. The issue is that while
			// image manifests report "arm" as the architecture, the node reports
			// something like "armv7l" (includes the variant), which causes arm images
			// to stop working with swarm mode. This patch removes the architecture
			// constraint for arm images to ensure tasks get scheduled.
			arch := p.Architecture
			if strings.ToLower(arch) == "arm" {
				arch = ""
			}
			platforms = append(platforms, swarm.Platform{
				Architecture: arch,
				OS:           p.OS,
			})
		}
	}
	return imageWithDigest, platforms, err
}

// imageWithDigestString takes an image string and a digest, and updates
// the image string if it didn't originally contain a digest. It returns
// image unmodified in other situations.
func imageWithDigestString(image string, dgst digest.Digest) string {
	namedRef, err := reference.ParseNormalizedNamed(image)
	if err == nil {
		if _, isCanonical := namedRef.(reference.Canonical); !isCanonical {
			// ensure that image gets a default tag if none is provided
			img, err := reference.WithDigest(namedRef, dgst)
			if err == nil {
				return reference.FamiliarString(img)
			}
		}
	}
	return image
}

// imageWithTagString takes an image string, and returns a tagged image
// string, adding a 'latest' tag if one was not provided. It returns an
// empty string if a canonical reference was provided
func imageWithTagString(image string) string {
	namedRef, err := reference.ParseNormalizedNamed(image)
	if err == nil {
		return reference.FamiliarString(reference.TagNameOnly(namedRef))
	}
	return ""
}

// digestWarning constructs a formatted warning string using the
// image name that could not be pinned by digest. The formatting
// is hardcoded, but could me made smarter in the future
func digestWarning(image string) string {
	return fmt.Sprintf("image %s could not be accessed on a registry to record\nits digest. Each node will access %s independently,\npossibly leading to different nodes running different\nversions of the image.\n", image, image)
}

func validateServiceSpec(s swarm.ServiceSpec) error {
	if s.TaskTemplate.ContainerSpec != nil && s.TaskTemplate.PluginSpec != nil {
		return errors.New("must not specify both a container spec and a plugin spec in the task template")
	}
	if s.TaskTemplate.PluginSpec != nil && s.TaskTemplate.Runtime != swarm.RuntimePlugin {
		return errors.New("mismatched runtime with plugin spec")
	}
	if s.TaskTemplate.ContainerSpec != nil && (s.TaskTemplate.Runtime != "" && s.TaskTemplate.Runtime != swarm.RuntimeContainer) {
		return errors.New("mismatched runtime with container spec")
	}
	return nil
}

func validateAPIVersion(c swarm.ServiceSpec, apiVersion string) error {
	for _, m := range c.TaskTemplate.ContainerSpec.Mounts {
		if m.BindOptions != nil {
			if m.BindOptions.NonRecursive && versions.LessThan(apiVersion, "1.40") {
				return errors.Errorf("bind-recursive=disabled requires API v1.40 or later")
			}
			// ReadOnlyNonRecursive can be safely ignored when API < 1.44
			if m.BindOptions.ReadOnlyForceRecursive && versions.LessThan(apiVersion, "1.44") {
				return errors.Errorf("bind-recursive=readonly requires API v1.44 or later")
			}
		}
	}
	return nil
}
