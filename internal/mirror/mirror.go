package mirror

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"path"

	"github.com/go-logr/logr"
	"github.com/spf13/afero"

	"github.com/xenitab/spegel/internal/registry"
)

// AddMirrorConfiguration sets up registry configuration to direct pulls through mirror.
// Refer to containerd registry configuration documentation for mor information about required configuration.
// https://github.com/containerd/containerd/blob/main/docs/cri/config.md#registry-configuration
// https://github.com/containerd/containerd/blob/main/docs/hosts.md#registry-configuration---examples
func AddMirrorConfiguration(ctx context.Context, fs afero.Fs, configPath string, registryURLs, mirrorURLs []url.URL) error {
	if err := validate(registryURLs); err != nil {
		return err
	}
	for _, registryURL := range registryURLs {
		content := hostsFileContent(registryURL, mirrorURLs)
		fp := path.Join(configPath, registryURL.Host, "hosts.toml")
		err := fs.MkdirAll(path.Dir(fp), 0755)
		if err != nil {
			return err
		}
		err = afero.WriteFile(fs, fp, []byte(content), 0644)
		if err != nil {
			return err
		}
		logr.FromContextOrDiscard(ctx).Info("added containerd mirror configuration", "registry", registryURL.String(), "path", fp)
	}
	return nil
}

// RemoveMirrorConfiguration removes all mirror configuration for all registries passed in the list.
func RemoveMirrorConfiguration(ctx context.Context, fs afero.Fs, configPath string, registryURLs []url.URL) error {
	errs := []error{}
	for _, registryURL := range registryURLs {
		dp := path.Join(configPath, registryURL.Host)
		err := fs.RemoveAll(dp)
		if err != nil {
			errs = append(errs, err)
			continue
		}
		logr.FromContextOrDiscard(ctx).Info("removed containerd mirror configuration", "registry", registryURL.String(), "path", dp)
	}
	return errors.Join(errs...)
}

func hostsFileContent(registryURL url.URL, mirrorURLs []url.URL) string {
	server := registryURL.String()
	if isDockerHub(registryURL) {
		server = "https://registry-1.docker.io"
	}
	content := fmt.Sprintf(`server = "%s"`, server)
	for i, mirrorURL := range mirrorURLs {
		content = fmt.Sprintf(`%[1]s

[host."%[3]s"]
  capabilities = ["pull", "resolve"]
[host."%[3]s".header]
  %[4]s = ["%[2]s"]
  %[5]s = ["true"]`, content, registryURL.String(), mirrorURL.String(), registry.RegistryHeader, registry.MirrorHeader)

		// We assume first mirror registry is local. All others are external.
		if i != 0 {
			content = fmt.Sprintf(`%s
  %s = ["true"]`, content, registry.ExternalHeader)
		}
	}
	return content
}

func isDockerHub(registryURL url.URL) bool {
	return registryURL.String() == "https://docker.io"
}

func validate(urls []url.URL) error {
	errs := []error{}
	for _, u := range urls {
		if u.Scheme != "http" && u.Scheme != "https" {
			errs = append(errs, fmt.Errorf("invalid registry url scheme must be http or https: %s", u.String()))
		}
		if u.Path != "" {
			errs = append(errs, fmt.Errorf("invalid registry url path has to be empty: %s", u.String()))
		}
		if len(u.Query()) != 0 {
			errs = append(errs, fmt.Errorf("invalid registry url query has to be empty: %s", u.String()))
		}
		if u.User != nil {
			errs = append(errs, fmt.Errorf("invalid registry url user has to be empty: %s", u.String()))
		}
	}
	return errors.Join(errs...)
}
