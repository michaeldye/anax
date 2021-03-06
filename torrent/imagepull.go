package torrent

import (
	docker "github.com/fsouza/go-dockerclient"
	"github.com/open-horizon/horizon-pkg-fetch/fetcherrors"
	"strings"

	"fmt"
	"github.com/golang/glog"
	"github.com/open-horizon/anax/config"
	"github.com/open-horizon/anax/containermessage"
	"os"
	"time"
)

const (
	pullAttemptDelayS = 15

	maxPullAttempts = 3
)

func dockerCredsFromConfigFile(configFilePath string) (*docker.AuthConfigurations, error) {

	f, err := os.Open(configFilePath)
	defer f.Close()
	if err != nil {
		return nil, err
	}

	auths, err := docker.NewAuthConfigurations(f)
	if err != nil {
		return nil, err
	}

	return auths, nil
}

func pullImageFromRepos(config config.Config, authConfigs *docker.AuthConfigurations, client *docker.Client, skipPartFetchFn *func(repotag string) (bool, error), deploymentDesc *containermessage.DeploymentDescription) error {

	// auth from creds file
	file_name := ""
	if config.DockerCredFilePath != "" {
		file_name = config.DockerCredFilePath
	} else {
		// if the config does not exist, use default provided the default file is there
		default_cred_fn := "/root/.docker/config.json"
		if _, err := os.Stat(default_cred_fn); err == nil {
			file_name = default_cred_fn
		}
	}

	if file_name != "" {
		glog.V(5).Infof("Using auth config file: %v", file_name)
		authFromFile, err := dockerCredsFromConfigFile(file_name)
		if err != nil {
			glog.Errorf("Failed to read creds file %v. Error: %v", file_name, err)
		} else {
			// do not overwrite incoming authconfigs entries, only augment them
			for k, v := range authFromFile.Configs {
				if _, exists := authConfigs.Configs[k]; !exists {
					authConfigs.Configs[k] = v
				}
			}
		}
	}

	// TODO: can we fetch in parallel with the docker client? If so, lift pattern from https://github.com/open-horizon/horizon-pkg-fetch/blob/master/fetch.go#L350
	for name, service := range deploymentDesc.Services {
		var pullAttempts int

		glog.Infof("Pulling image %v for service %v", service.Image, name)
		imageNameParts := strings.Split(service.Image, ":")

		// TODO: check the on-disk image to make sure it still verifies
		// N.B. It's possible to specify an outputstream here which means we could fetch a docker image and hash it, check the sig like we used to
		opts := docker.PullImageOptions{
			Repository: imageNameParts[0],
			Tag:        imageNameParts[1],
		}

		var auth docker.AuthConfiguration
		for domainName, creds := range authConfigs.Configs {
			repName := strings.Split(imageNameParts[0], "/")
			if repName[0] == domainName {
				auth = creds
			}
		}

		for pullAttempts <= maxPullAttempts {
			if err := client.PullImage(opts, auth); err == nil {
				glog.Infof("Succeeded fetching image %v for service %v", service.Image, name)
				break
			} else {
				glog.Errorf("Docker image pull(s) failed. Waiting %d seconds before retry. Error: %v", pullAttemptDelayS, err)
				pullAttempts++

				if pullAttempts != maxPullAttempts {
					time.Sleep(pullAttemptDelayS * time.Second)
				} else {
					msg := fmt.Sprintf("Max pull attempts reached (%d). Aborting fetch of Docker image %v", pullAttempts, service.Image)

					switch err.(type) {
					case *docker.Error:
						dErr := err.(*docker.Error)
						if dErr.Status == 500 && strings.Contains(dErr.Message, "cred") {
							return fetcherrors.PkgSourceFetchAuthError{Msg: msg, InternalError: dErr}
						} else {
							glog.Infof("Docker client error occurred %v", err)
							return err
						}

					default:
						glog.Errorf("(Unknown error type, %T) Internal error of unidentifiable type: %v. Original: %v", err, msg, err)
						return err

					}
				}
			}
		}

	}

	return nil
}
