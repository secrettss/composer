// +build local

/*
   Copyright 2020 Docker Compose CLI authors

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package local

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"github.com/compose-spec/compose-go/cli"
	"github.com/compose-spec/compose-go/types"
	"github.com/docker/buildx/build"
	"github.com/docker/cli/cli/config"
	"github.com/docker/distribution/reference"
	moby "github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/filters"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/strslice"
	mobyvolume "github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/docker/docker/errdefs"
	"github.com/docker/docker/pkg/jsonmessage"
	"github.com/docker/docker/pkg/stdcopy"
	"github.com/docker/docker/registry"
	"github.com/docker/go-connections/nat"
	"github.com/pkg/errors"
	"github.com/sanathkr/go-yaml"
	"golang.org/x/sync/errgroup"

	"github.com/docker/compose-cli/api/compose"
	"github.com/docker/compose-cli/formatter"
	"github.com/docker/compose-cli/progress"
)

type composeService struct {
	apiClient *client.Client
}

func (s *composeService) Build(ctx context.Context, project *types.Project) error {
	opts := map[string]build.Options{}
	for _, service := range project.Services {
		if service.Build != nil {
			opts[service.Name] = s.toBuildOptions(service, project.WorkingDir)
		}
	}

	return s.build(ctx, project, opts)
}

func (s *composeService) Push(ctx context.Context, project *types.Project) error {
	configFile, err := config.Load(config.Dir())
	if err != nil {
		return err
	}
	eg, ctx := errgroup.WithContext(ctx)

	info, err := s.apiClient.Info(ctx)
	if err != nil {
		return err
	}
	if info.IndexServerAddress == "" {
		info.IndexServerAddress = registry.IndexServer
	}

	for _, service := range project.Services {
		if service.Build == nil {
			continue
		}
		service := service
		eg.Go(func() error {
			w := progress.ContextWriter(ctx)

			ref, err := reference.ParseNormalizedNamed(service.Image)
			if err != nil {
				return err
			}

			repoInfo, err := registry.ParseRepositoryInfo(ref)
			if err != nil {
				return err
			}

			key := repoInfo.Index.Name
			if repoInfo.Index.Official {
				key = info.IndexServerAddress
			}
			authConfig, err := configFile.GetAuthConfig(key)
			if err != nil {
				return err
			}

			buf, err := json.Marshal(authConfig)
			if err != nil {
				return err
			}

			stream, err := s.apiClient.ImagePush(ctx, service.Image, moby.ImagePushOptions{
				RegistryAuth: base64.URLEncoding.EncodeToString(buf),
			})
			if err != nil {
				return err
			}
			dec := json.NewDecoder(stream)
			for {
				var jm jsonmessage.JSONMessage
				if err := dec.Decode(&jm); err != nil {
					if err == io.EOF {
						break
					}
					return err
				}
				if jm.Error != nil {
					return errors.New(jm.Error.Message)
				}
				toProgressEvent(service.Name, jm, w)
			}
			return nil
		})
	}
	return eg.Wait()
}

func toProgressEvent(prefix string, jm jsonmessage.JSONMessage, w progress.Writer) {
	if jm.ID == "" {
		// skipped
		return
	}
	var (
		text   string
		status = progress.Working
	)
	if jm.Status == "Pull complete" || jm.Status == "Already exists" {
		status = progress.Done
	}
	if jm.Error != nil {
		status = progress.Error
		text = jm.Error.Message
	}
	if jm.Progress != nil {
		text = jm.Progress.String()
	}
	w.Event(progress.Event{
		ID:         fmt.Sprintf("Pushing %s: %s", prefix, jm.ID),
		Text:       jm.Status,
		Status:     status,
		StatusText: text,
	})
}

func (s *composeService) Up(ctx context.Context, project *types.Project, detach bool) error {
	err := s.ensureImagesExists(ctx, project)
	if err != nil {
		return err
	}
	for k, network := range project.Networks {
		if !network.External.External && network.Name == k {
			network.Name = fmt.Sprintf("%s_%s", project.Name, k)
			project.Networks[k] = network
		}
		network.Labels = network.Labels.Add(networkLabel, k)
		network.Labels = network.Labels.Add(projectLabel, project.Name)
		network.Labels = network.Labels.Add(versionLabel, ComposeVersion)
		err := s.ensureNetwork(ctx, network)
		if err != nil {
			return err
		}
	}

	for k, volume := range project.Volumes {
		if !volume.External.External && volume.Name != "" {
			volume.Name = fmt.Sprintf("%s_%s", project.Name, k)
			project.Volumes[k] = volume
		}
		volume.Labels = volume.Labels.Add(volumeLabel, k)
		volume.Labels = volume.Labels.Add(projectLabel, project.Name)
		volume.Labels = volume.Labels.Add(versionLabel, ComposeVersion)
		err := s.ensureVolume(ctx, volume)
		if err != nil {
			return err
		}
	}

	err = InDependencyOrder(ctx, project, func(c context.Context, service types.ServiceConfig) error {
		return s.ensureService(c, project, service)
	})
	return err
}

func getContainerName(c moby.Container) string {
	// Names return container canonical name /foo  + link aliases /linked_by/foo
	for _, name := range c.Names {
		if strings.LastIndex(name, "/") == 0 {
			return name[1:]
		}
	}
	return c.Names[0][1:]
}

func (s *composeService) Down(ctx context.Context, projectName string) error {
	eg, _ := errgroup.WithContext(ctx)
	w := progress.ContextWriter(ctx)

	project, err := s.projectFromContainerLabels(ctx, projectName)
	if err != nil || project == nil {
		return err
	}

	err = InReverseDependencyOrder(ctx, project, func(c context.Context, service types.ServiceConfig) error {
		filter := filters.NewArgs(projectFilter(project.Name), serviceFilter(service.Name))
		return s.removeContainers(ctx, w, eg, filter)
	})

	if err != nil {
		return err
	}
	err = eg.Wait()
	if err != nil {
		return err
	}
	networks, err := s.apiClient.NetworkList(ctx, moby.NetworkListOptions{
		Filters: filters.NewArgs(
			projectFilter(projectName),
		),
	})
	if err != nil {
		return err
	}
	for _, n := range networks {
		networkID := n.ID
		networkName := n.Name
		eg.Go(func() error {
			return s.ensureNetworkDown(ctx, networkID, networkName)
		})
	}

	return eg.Wait()
}

func (s *composeService) removeContainers(ctx context.Context, w progress.Writer, eg *errgroup.Group, filter filters.Args) error {
	containers, err := s.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filter,
	})
	if err != nil {
		return err
	}
	for _, container := range containers {
		eg.Go(func() error {
			eventName := "Container " + getContainerName(container)
			w.Event(progress.StoppingEvent(eventName))
			err := s.apiClient.ContainerStop(ctx, container.ID, nil)
			if err != nil {
				w.Event(progress.ErrorMessageEvent(eventName, "Error while Stopping"))
				return err
			}
			w.Event(progress.RemovingEvent(eventName))
			err = s.apiClient.ContainerRemove(ctx, container.ID, moby.ContainerRemoveOptions{})
			if err != nil {
				w.Event(progress.ErrorMessageEvent(eventName, "Error while Removing"))
				return err
			}
			w.Event(progress.RemovedEvent(eventName))
			return nil
		})
	}
	return nil
}

func (s *composeService) projectFromContainerLabels(ctx context.Context, projectName string) (*types.Project, error) {
	containers, err := s.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			projectFilter(projectName),
		),
	})
	if err != nil {
		return nil, err
	}
	if len(containers) == 0 {
		return nil, nil
	}
	options, err := loadProjectOptionsFromLabels(containers[0])
	if err != nil {
		return nil, err
	}
	if options.ConfigPaths[0] == "-" {
		fakeProject := &types.Project{
			Name: projectName,
		}
		for _, container := range containers {
			fakeProject.Services = append(fakeProject.Services, types.ServiceConfig{
				Name: container.Labels[serviceLabel],
			})
		}
		return fakeProject, nil
	}
	project, err := cli.ProjectFromOptions(options)
	if err != nil {
		return nil, err
	}

	return project, nil
}

func loadProjectOptionsFromLabels(c moby.Container) (*cli.ProjectOptions, error) {
	var configFiles []string
	relativePathConfigFiles := strings.Split(c.Labels[configFilesLabel], ",")
	for _, c := range relativePathConfigFiles {
		configFiles = append(configFiles, filepath.Base(c))
	}
	return cli.NewProjectOptions(configFiles,
		cli.WithOsEnv,
		cli.WithWorkingDirectory(c.Labels[workingDirLabel]),
		cli.WithName(c.Labels[projectLabel]))
}

func (s *composeService) Logs(ctx context.Context, projectName string, w io.Writer) error {
	list, err := s.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			projectFilter(projectName),
		),
	})
	if err != nil {
		return err
	}
	consumer := formatter.NewLogConsumer(w)
	eg, ctx := errgroup.WithContext(ctx)
	for _, c := range list {
		service := c.Labels[serviceLabel]
		container, err := s.apiClient.ContainerInspect(ctx, c.ID)
		if err != nil {
			return err
		}

		eg.Go(func() error {
			r, err := s.apiClient.ContainerLogs(ctx, container.ID, moby.ContainerLogsOptions{
				ShowStdout: true,
				ShowStderr: true,
				Follow:     true,
			})
			defer r.Close() // nolint errcheck

			if err != nil {
				return err
			}
			w := consumer.GetWriter(service, container.ID)
			if container.Config.Tty {
				_, err = io.Copy(w, r)
			} else {
				_, err = stdcopy.StdCopy(w, w, r)
			}
			return err
		})
	}
	return eg.Wait()
}

func (s *composeService) Ps(ctx context.Context, projectName string) ([]compose.ServiceStatus, error) {
	list, err := s.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(
			projectFilter(projectName),
		),
	})
	if err != nil {
		return nil, err
	}
	return containersToServiceStatus(list)
}

func containersToServiceStatus(containers []moby.Container) ([]compose.ServiceStatus, error) {
	containersByLabel, keys, err := groupContainerByLabel(containers, serviceLabel)
	if err != nil {
		return nil, err
	}
	var services []compose.ServiceStatus
	for _, service := range keys {
		containers := containersByLabel[service]
		runnningContainers := []moby.Container{}
		for _, container := range containers {
			if container.State == "running" {
				runnningContainers = append(runnningContainers, container)
			}
		}
		services = append(services, compose.ServiceStatus{
			ID:       service,
			Name:     service,
			Desired:  len(containers),
			Replicas: len(runnningContainers),
		})
	}
	return services, nil
}

func groupContainerByLabel(containers []moby.Container, labelName string) (map[string][]moby.Container, []string, error) {
	containersByLabel := map[string][]moby.Container{}
	keys := []string{}
	for _, c := range containers {
		label, ok := c.Labels[labelName]
		if !ok {
			return nil, nil, fmt.Errorf("No label %q set on container %q of compose project", labelName, c.ID)
		}
		labelContainers, ok := containersByLabel[label]
		if !ok {
			labelContainers = []moby.Container{}
			keys = append(keys, label)
		}
		labelContainers = append(labelContainers, c)
		containersByLabel[label] = labelContainers
	}
	sort.Strings(keys)
	return containersByLabel, keys, nil
}

func (s *composeService) List(ctx context.Context, projectName string) ([]compose.Stack, error) {
	list, err := s.apiClient.ContainerList(ctx, moby.ContainerListOptions{
		Filters: filters.NewArgs(hasProjectLabelFilter()),
	})
	if err != nil {
		return nil, err
	}

	return containersToStacks(list)
}

func containersToStacks(containers []moby.Container) ([]compose.Stack, error) {
	containersByLabel, keys, err := groupContainerByLabel(containers, projectLabel)
	if err != nil {
		return nil, err
	}
	var projects []compose.Stack
	for _, project := range keys {
		projects = append(projects, compose.Stack{
			ID:     project,
			Name:   project,
			Status: combinedStatus(containerToState(containersByLabel[project])),
		})
	}
	return projects, nil
}

func containerToState(containers []moby.Container) []string {
	statuses := []string{}
	for _, c := range containers {
		statuses = append(statuses, c.State)
	}
	return statuses
}

func combinedStatus(statuses []string) string {
	nbByStatus := map[string]int{}
	keys := []string{}
	for _, status := range statuses {
		nb, ok := nbByStatus[status]
		if !ok {
			nb = 0
			keys = append(keys, status)
		}
		nbByStatus[status] = nb + 1
	}
	sort.Strings(keys)
	result := ""
	for _, status := range keys {
		nb := nbByStatus[status]
		if result != "" {
			result = result + ", "
		}
		result = result + fmt.Sprintf("%s(%d)", status, nb)
	}
	return result
}

func (s *composeService) Convert(ctx context.Context, project *types.Project, format string) ([]byte, error) {
	switch format {
	case "json":
		return json.MarshalIndent(project, "", "  ")
	case "yaml":
		return yaml.Marshal(project)
	default:
		return nil, fmt.Errorf("unsupported format %q", format)
	}
}

func getContainerCreateOptions(p *types.Project, s types.ServiceConfig, number int, inherit *moby.Container) (*container.Config, *container.HostConfig, *network.NetworkingConfig, error) {
	hash, err := jsonHash(s)
	if err != nil {
		return nil, nil, nil, err
	}
	// TODO: change oneoffLabel value for containers started with `docker compose run`
	labels := map[string]string{
		projectLabel:         p.Name,
		serviceLabel:         s.Name,
		versionLabel:         ComposeVersion,
		oneoffLabel:          "False",
		configHashLabel:      hash,
		workingDirLabel:      p.WorkingDir,
		configFilesLabel:     strings.Join(p.ComposeFiles, ","),
		containerNumberLabel: strconv.Itoa(number),
	}

	var (
		runCmd     strslice.StrSlice
		entrypoint strslice.StrSlice
	)
	if len(s.Command) > 0 {
		runCmd = strslice.StrSlice(s.Command)
	}
	if len(s.Entrypoint) > 0 {
		entrypoint = strslice.StrSlice(s.Entrypoint)
	}
	image := s.Image
	if s.Image == "" {
		image = fmt.Sprintf("%s_%s", p.Name, s.Name)
	}

	var (
		tty         = s.Tty
		stdinOpen   = s.StdinOpen
		attachStdin = false
	)

	containerConfig := container.Config{
		Hostname:        s.Hostname,
		Domainname:      s.DomainName,
		User:            s.User,
		ExposedPorts:    buildContainerPorts(s),
		Tty:             tty,
		OpenStdin:       stdinOpen,
		StdinOnce:       true,
		AttachStdin:     attachStdin,
		AttachStderr:    true,
		AttachStdout:    true,
		Cmd:             runCmd,
		Image:           image,
		WorkingDir:      s.WorkingDir,
		Entrypoint:      entrypoint,
		NetworkDisabled: s.NetworkMode == "disabled",
		MacAddress:      s.MacAddress,
		Labels:          labels,
		StopSignal:      s.StopSignal,
		Env:             toMobyEnv(s.Environment),
		Healthcheck:     toMobyHealthCheck(s.HealthCheck),
		// Volumes:         // FIXME unclear to me the overlap with HostConfig.Mounts
		StopTimeout: toSeconds(s.StopGracePeriod),
	}

	mountOptions := buildContainerMountOptions(p, s, inherit)
	bindings := buildContainerBindingOptions(s)

	networkMode := getNetworkMode(p, s)
	hostConfig := container.HostConfig{
		Mounts:         mountOptions,
		CapAdd:         strslice.StrSlice(s.CapAdd),
		CapDrop:        strslice.StrSlice(s.CapDrop),
		NetworkMode:    networkMode,
		Init:           s.Init,
		ReadonlyRootfs: s.ReadOnly,
		// ShmSize: , TODO
		Sysctls:      s.Sysctls,
		PortBindings: bindings,
	}

	networkConfig := buildDefaultNetworkConfig(s, networkMode)
	return &containerConfig, &hostConfig, networkConfig, nil
}

func buildContainerPorts(s types.ServiceConfig) nat.PortSet {
	ports := nat.PortSet{}
	for _, p := range s.Ports {
		p := nat.Port(fmt.Sprintf("%d/%s", p.Target, p.Protocol))
		ports[p] = struct{}{}
	}
	return ports
}

func buildContainerBindingOptions(s types.ServiceConfig) nat.PortMap {
	bindings := nat.PortMap{}
	for _, port := range s.Ports {
		p := nat.Port(fmt.Sprintf("%d/%s", port.Target, port.Protocol))
		bind := []nat.PortBinding{}
		binding := nat.PortBinding{}
		if port.Published > 0 {
			binding.HostPort = fmt.Sprint(port.Published)
		}
		bind = append(bind, binding)
		bindings[p] = bind
	}
	return bindings
}

func buildContainerMountOptions(p *types.Project, s types.ServiceConfig, inherit *moby.Container) []mount.Mount {
	mounts := []mount.Mount{}
	var inherited []string
	if inherit != nil {
		for _, m := range inherit.Mounts {
			if m.Type == "tmpfs" {
				continue
			}
			src := m.Source
			if m.Type == "volume" {
				src = m.Name
			}
			mounts = append(mounts, mount.Mount{
				Type:     m.Type,
				Source:   src,
				Target:   m.Destination,
				ReadOnly: !m.RW,
			})
			inherited = append(inherited, m.Destination)
		}
	}

	for _, v := range s.Volumes {
		if contains(inherited, v.Target) {
			continue
		}
		source := v.Source
		if v.Type == "bind" && !filepath.IsAbs(source) {
			// FIXME handle ~/
			source = filepath.Join(p.WorkingDir, source)
		}

		mounts = append(mounts, mount.Mount{
			Type:          mount.Type(v.Type),
			Source:        source,
			Target:        v.Target,
			ReadOnly:      v.ReadOnly,
			Consistency:   mount.Consistency(v.Consistency),
			BindOptions:   buildBindOption(v.Bind),
			VolumeOptions: buildVolumeOptions(v.Volume),
			TmpfsOptions:  buildTmpfsOptions(v.Tmpfs),
		})
	}
	return mounts
}

func buildBindOption(bind *types.ServiceVolumeBind) *mount.BindOptions {
	if bind == nil {
		return nil
	}
	return &mount.BindOptions{
		Propagation: mount.Propagation(bind.Propagation),
		// NonRecursive: false, FIXME missing from model ?
	}
}

func buildVolumeOptions(vol *types.ServiceVolumeVolume) *mount.VolumeOptions {
	if vol == nil {
		return nil
	}
	return &mount.VolumeOptions{
		NoCopy: vol.NoCopy,
		// Labels:       , // FIXME missing from model ?
		// DriverConfig: , // FIXME missing from model ?
	}
}

func buildTmpfsOptions(tmpfs *types.ServiceVolumeTmpfs) *mount.TmpfsOptions {
	if tmpfs == nil {
		return nil
	}
	return &mount.TmpfsOptions{
		SizeBytes: tmpfs.Size,
		// Mode:      , // FIXME missing from model ?
	}
}

func buildDefaultNetworkConfig(s types.ServiceConfig, networkMode container.NetworkMode) *network.NetworkingConfig {
	config := map[string]*network.EndpointSettings{}
	net := string(networkMode)
	config[net] = &network.EndpointSettings{
		Aliases: getAliases(s, s.Networks[net]),
	}

	return &network.NetworkingConfig{
		EndpointsConfig: config,
	}
}

func getAliases(s types.ServiceConfig, c *types.ServiceNetworkConfig) []string {
	aliases := []string{s.Name}
	if c != nil {
		aliases = append(aliases, c.Aliases...)
	}
	return aliases
}

func getNetworkMode(p *types.Project, service types.ServiceConfig) container.NetworkMode {
	mode := service.NetworkMode
	if mode == "" {
		if len(p.Networks) > 0 {
			for name := range getNetworksForService(service) {
				return container.NetworkMode(p.Networks[name].Name)
			}
		}
		return container.NetworkMode("none")
	}

	// FIXME incomplete implementation
	if strings.HasPrefix(mode, "service:") {
		panic("Not yet implemented")
	}
	if strings.HasPrefix(mode, "container:") {
		panic("Not yet implemented")
	}

	return container.NetworkMode(mode)
}

func getNetworksForService(s types.ServiceConfig) map[string]*types.ServiceNetworkConfig {
	if len(s.Networks) > 0 {
		return s.Networks
	}
	return map[string]*types.ServiceNetworkConfig{"default": nil}
}

func (s *composeService) ensureNetwork(ctx context.Context, n types.NetworkConfig) error {
	_, err := s.apiClient.NetworkInspect(ctx, n.Name, moby.NetworkInspectOptions{})
	if err != nil {
		if errdefs.IsNotFound(err) {
			if n.External.External {
				return fmt.Errorf("network %s declared as external, but could not be found", n.Name)
			}
			createOpts := moby.NetworkCreate{
				// TODO NameSpace Labels
				Labels:     n.Labels,
				Driver:     n.Driver,
				Options:    n.DriverOpts,
				Internal:   n.Internal,
				Attachable: n.Attachable,
			}

			if n.Ipam.Driver != "" || len(n.Ipam.Config) > 0 {
				createOpts.IPAM = &network.IPAM{}
			}

			if n.Ipam.Driver != "" {
				createOpts.IPAM.Driver = n.Ipam.Driver
			}

			for _, ipamConfig := range n.Ipam.Config {
				config := network.IPAMConfig{
					Subnet: ipamConfig.Subnet,
				}
				createOpts.IPAM.Config = append(createOpts.IPAM.Config, config)
			}
			networkEventName := fmt.Sprintf("Network %q", n.Name)
			w := progress.ContextWriter(ctx)
			w.Event(progress.CreatingEvent(networkEventName))
			if _, err := s.apiClient.NetworkCreate(ctx, n.Name, createOpts); err != nil {
				w.Event(progress.ErrorEvent(networkEventName))
				return errors.Wrapf(err, "failed to create network %s", n.Name)
			}
			w.Event(progress.CreatedEvent(networkEventName))
			return nil
		}
		return err
	}
	return nil
}

func (s *composeService) ensureNetworkDown(ctx context.Context, networkID string, networkName string) error {
	w := progress.ContextWriter(ctx)
	eventName := fmt.Sprintf("Network %q", networkName)
	w.Event(progress.RemovingEvent(eventName))

	if err := s.apiClient.NetworkRemove(ctx, networkID); err != nil {
		w.Event(progress.ErrorEvent(eventName))
		return errors.Wrapf(err, fmt.Sprintf("failed to create network %s", networkID))
	}

	w.Event(progress.RemovedEvent(eventName))
	return nil
}

func (s *composeService) ensureVolume(ctx context.Context, volume types.VolumeConfig) error {
	// TODO could identify volume by label vs name
	_, err := s.apiClient.VolumeInspect(ctx, volume.Name)
	if err != nil {
		if errdefs.IsNotFound(err) {
			eventName := fmt.Sprintf("Volume %q", volume.Name)
			w := progress.ContextWriter(ctx)
			w.Event(progress.CreatingEvent(eventName))
			// TODO we miss support for driver_opts and labels
			_, err := s.apiClient.VolumeCreate(ctx, mobyvolume.VolumeCreateBody{
				Labels:     volume.Labels,
				Name:       volume.Name,
				Driver:     volume.Driver,
				DriverOpts: volume.DriverOpts,
			})
			if err != nil {
				w.Event(progress.ErrorEvent(eventName))
				return err
			}
			w.Event(progress.CreatedEvent(eventName))
		}
		return err
	}
	return nil
}