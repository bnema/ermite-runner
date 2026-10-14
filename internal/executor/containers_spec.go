// SPDX-License-Identifier: MIT

package executor

import (
	"context"
	"fmt"
	"strings"

	"bnema.dev/ermite-actions/expr"
	"bnema.dev/ermite-actions/template"
	"github.com/google/uuid"
)

// This file is the model of the containers of a job (the job container and
// the service containers): ContainerInfo.cs and the evaluation of
// jobs.<id>.container and jobs.<id>.services
// (PipelineTemplateConverter.ConvertToJobContainer, :248-352).

// mount is MountVolume (ContainerInfo.cs:246-318).
type mount struct {
	source, target string
	readOnly       bool
	// user is the text the workflow gave (UserProvidedValue), empty for the
	// mounts of the runner.
	user string
}

// parseVolume is MountVolume.ParseVolumeString (ContainerInfo.cs:283-318):
// "source:target[:ro]", "target:ro" or "target". The runner does not look at
// the text further; docker rejects what is not valid.
func parseVolume(v string) mount {
	parts := strings.Split(v, ":")
	switch len(parts) {
	case 3:
		return mount{source: parts[0], target: parts[1], readOnly: strings.EqualFold(parts[2], "ro")}
	case 2:
		if strings.EqualFold(parts[1], "ro") {
			return mount{target: parts[0], readOnly: true}
		}
		return mount{source: parts[0], target: parts[1]}
	}
	return mount{target: v}
}

// portMapping is PortMapping: a published port as `docker port` shows it.
type portMapping struct{ host, container, proto string }

// container is ContainerInfo: what the workflow asked for and what is known
// of the container once it runs.
type container struct {
	isJob bool
	// alias is the network alias of a service (its key in services:).
	alias string
	// name is the container name (ContainerDisplayName).
	name                                  string
	image, options, entrypoint, entryArgs string
	env                                   *ordered
	ports                                 []string // UserPortMappings, in order, without duplicates
	mounts                                []mount
	credUser, credPass                    string
	registryServer                        string
	workDir                               string
	id, network, runtimePath              string
	failedInit                            bool
	maps                                  []pathMap // path translation, first match wins
	portMappings                          []portMapping
}

// userMounts are the mounts the workflow listed.
func (c *container) userMounts() []mount {
	var out []mount
	for _, m := range c.mounts {
		if m.user != "" {
			out = append(out, m)
		}
	}
	return out
}

// translateToContainer is TranslateToContainerPath (ContainerInfo.cs:190-228,
// Linux): a path equal to a mapped host directory, or below it.
func (c *container) translateToContainer(p string) string {
	if p == "" {
		return p
	}
	for _, m := range c.maps {
		if p == m.host {
			return m.container
		}
		if strings.HasPrefix(p, m.host+"/") {
			return m.container + p[len(m.host):]
		}
	}
	return p
}

// translateToHost is TranslateToHostPath (ContainerInfo.cs:230-268).
func (c *container) translateToHost(p string) string {
	if p == "" {
		return p
	}
	for _, m := range c.maps {
		if p == m.container {
			return m.host
		}
		if strings.HasPrefix(p, m.container+"/") {
			return m.host + p[len(m.container):]
		}
	}
	return p
}

// resolvePathForStepHost is ContainerStepHost.ResolvePathForStepHost
// (StepHost.cs:127-152): the path is translated when it starts with the host
// path of one of the mounts (a plain string prefix, as in the runner).
func (c *container) resolvePath(p string) string {
	p = strings.Trim(p, `"`)
	for _, m := range c.mounts {
		if m.source != "" && strings.HasPrefix(p, m.source) {
			return c.translateToContainer(p)
		}
	}
	return p
}

// parseRegistryHost is DockerUtil.ParseRegistryHostnameFromImageName
// (DockerUtil.cs:55-68).
func parseRegistryHost(image string) string {
	parts := strings.Split(image, "/")
	if len(parts) == 2 && strings.ContainsAny(parts[0], ":.") {
		return parts[0]
	}
	if len(parts) > 2 {
		return parts[0]
	}
	return ""
}

// containerName is the display name `<alias>_<sanitized image>_<6 hex>`
// (ContainerInfo.cs:44) with a fresh 32-hex alias.
func containerName(image string) string {
	return strings.ReplaceAll(uuid.NewString(), "-", "") + "_" + sanitizeName(image) + "_" +
		strings.ReplaceAll(uuid.NewString(), "-", "")[:6]
}

// newContainer is the constructor of ContainerInfo (ContainerInfo.cs:35-96).
func (j *job) newContainer(spec *containerSpec, isJob bool, alias string) *container {
	c := &container{
		isJob: isJob, alias: alias, image: spec.image, options: spec.options,
		entrypoint: spec.entrypoint, entryArgs: spec.command, env: newOrdered(),
		credUser: spec.credUser, credPass: spec.credPass, registryServer: parseRegistryHost(spec.image),
	}
	c.name = containerName(spec.image)
	for _, p := range spec.env {
		c.env.Set(p.Key, p.Value)
	}
	lay := j.state.Layout()
	// The order of the mappings is the runner's (ContainerInfo.cs:55-58).
	c.maps = []pathMap{{lay.WorkRoot, "/__w"}, {lay.ToolCache, "/__t"}, {j.externalsDir(), "/__e"}}
	if isJob {
		c.mounts = append(c.mounts, mount{source: "/var/run/docker.sock", target: "/var/run/docker.sock"})
	}
	for _, p := range spec.ports {
		if !containsString(c.ports, p) { // UserPortMappings[port] = port
			c.ports = append(c.ports, p)
		}
	}
	for _, v := range spec.volumes {
		m := parseVolume(v)
		m.user = v
		c.mounts = append(c.mounts, m)
	}
	c.proxyEnv(j.baseEnv)
	return c
}

func containsString(l []string, s string) bool {
	for _, x := range l {
		if x == s {
			return true
		}
	}
	return false
}

// proxyEnv is UpdateWebProxyEnv (ContainerInfo.cs:353-374): the proxy
// settings of the runner, unless the container sets them itself.
func (c *container) proxyEnv(base []string) {
	pick := func(names ...string) string {
		for _, n := range names {
			if v := strings.TrimSpace(envValue(base, n)); v != "" {
				return v
			}
		}
		return ""
	}
	add := func(v string, keys ...string) {
		if v == "" {
			return
		}
		for _, k := range keys {
			if _, ok := c.env.Get(k); !ok {
				c.env.Set(k, v)
			}
		}
	}
	add(pick("http_proxy", "HTTP_PROXY"), "HTTP_PROXY", "http_proxy")
	add(pick("https_proxy", "HTTPS_PROXY"), "HTTPS_PROXY", "https_proxy")
	add(pick("no_proxy", "NO_PROXY"), "NO_PROXY", "no_proxy")
}

// containerSpec is JobContainer: the evaluated container: or service.
type containerSpec struct {
	image, options, entrypoint, command string
	env                                 []Pair
	ports, volumes                      []string
	credUser, credPass                  string
}

// asContainerString is AssertString of the converter: scalars are strings.
func asContainerString(v expr.Value, what string) (string, error) {
	switch v.Kind() {
	case expr.KindArray, expr.KindDictionary:
		return "", ghErrorf("Unexpected type '%s' encountered while reading '%s'. The type 'StringToken' was expected.", v.Kind(), what)
	}
	return expr.CoerceString(v), nil
}

func containerStrings(v expr.Value, what string) ([]string, error) {
	a, ok := v.(*expr.Array)
	if !ok {
		return nil, ghErrorf("Unexpected type '%s' encountered while reading '%s'. The type 'SequenceToken' was expected.", v.Kind(), what)
	}
	var out []string
	for _, it := range a.Items() {
		s, err := asContainerString(it, what+" "+expr.CoerceString(it))
		if err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, nil
}

// serviceCommandAllowed is the feature flag of entrypoint: and command: of a
// service (ExecutionContext.cs:1545: actions_service_container_command or
// ACTIONS_SERVICE_CONTAINER_COMMAND; ermite has no feature flags).
func (j *job) serviceCommandAllowed() bool {
	return convertToBool(envValue(j.baseEnv, "ACTIONS_SERVICE_CONTAINER_COMMAND"))
}

// convertContainer is ConvertToJobContainer (PipelineTemplateConverter.cs:
// 248-352) on an evaluated value. nil means no container (an empty image).
func (j *job) convertContainer(v expr.Value, service bool) (*containerSpec, error) {
	spec := &containerSpec{}
	if d, ok := v.(*expr.Dictionary); ok {
		for _, p := range d.Pairs() {
			var err error
			switch p.Key {
			case "image":
				spec.image, err = asContainerString(p.Value, "container image")
			case "env":
				spec.env, err = containerEnv(p.Value)
			case "options":
				spec.options, err = asContainerString(p.Value, "container options")
			case "entrypoint", "command":
				if !service || !j.serviceCommandAllowed() {
					return nil, ghErrorf("The key '%s' is not allowed", p.Key)
				}
				s, e := asContainerString(p.Value, "container "+p.Key)
				err = e
				if p.Key == "entrypoint" {
					spec.entrypoint = s
				} else {
					spec.command = s
				}
			case "ports":
				spec.ports, err = containerStrings(p.Value, "container ports")
			case "volumes":
				spec.volumes, err = containerStrings(p.Value, "container volumes")
			case "credentials":
				err = containerCredentials(p.Value, spec)
			default:
				err = ghErrorf("Error while reading 'container key'. Unexpected value '%s'", p.Key)
			}
			if err != nil {
				return nil, err
			}
		}
	} else {
		s, err := asContainerString(v, "container")
		if err != nil {
			return nil, err
		}
		spec.image = s
	}
	spec.image = strings.TrimPrefix(spec.image, "docker://")
	if spec.image == "" {
		return nil, nil
	}
	// The image is one argument of docker; a leading "-" would make it an option.
	if strings.HasPrefix(spec.image, "-") {
		return nil, ghErrorf("Invalid container image '%s'", spec.image)
	}
	return spec, nil
}

func containerEnv(v expr.Value) ([]Pair, error) {
	d, ok := v.(*expr.Dictionary)
	if !ok {
		return nil, ghErrorf("Unexpected type '%s' encountered while reading 'container env'. The type 'MappingToken' was expected.", v.Kind())
	}
	var out []Pair
	for _, p := range d.Pairs() {
		s, err := asContainerString(p.Value, "container env "+p.Key)
		if err != nil {
			return nil, err
		}
		out = append(out, Pair{p.Key, s})
	}
	return out, nil
}

func containerCredentials(v expr.Value, spec *containerSpec) error {
	d, ok := v.(*expr.Dictionary)
	if !ok {
		return ghErrorf("Unexpected type '%s' encountered while reading 'credentials'. The type 'MappingToken' was expected.", v.Kind())
	}
	for _, p := range d.Pairs() {
		s, err := asContainerString(p.Value, p.Key)
		if err != nil {
			return err
		}
		switch p.Key {
		case "username":
			spec.credUser = s
		case "password":
			spec.credPass = s
		default:
			return ghErrorf("Error while reading 'credentials key %s'. Unexpected value '%s'", p.Key, p.Key)
		}
	}
	return nil
}

// evalContainers evaluates container: and services: of the job
// (JobExtension.cs:250-275) with the contexts the job has by then.
func (j *job) evalContainers() (job *container, services []*container, err error) {
	ctx := context.Background()
	j.mu.Lock()
	env := j.state.JobEnv()
	j.mu.Unlock()
	te := j.evalEnv(env)
	if t := j.jb.Container; t != nil {
		v, e := template.Evaluate(ctx, t, te)
		if e != nil {
			return nil, nil, e
		}
		if _, null := v.(expr.Null); !null {
			spec, e := j.convertContainer(v, false)
			if e != nil {
				return nil, nil, e
			}
			if spec != nil {
				job = j.newContainer(spec, true, "")
			}
		}
	}
	if t := j.jb.Services; t != nil {
		v, e := template.Evaluate(ctx, t, te)
		if e != nil {
			return nil, nil, e
		}
		d, ok := v.(*expr.Dictionary)
		if !ok {
			if _, null := v.(expr.Null); null {
				return job, nil, nil
			}
			return nil, nil, ghErrorf("Unexpected type '%s' encountered while reading 'services'. The type 'MappingToken' was expected.", v.Kind())
		}
		for _, p := range d.Pairs() {
			spec, e := j.convertContainer(p.Value, true)
			if e != nil {
				return nil, nil, e
			}
			if spec == nil {
				j.out(1, fmt.Sprintf("The service '%s' will not be started because the container definition has an empty image.", p.Key))
				continue
			}
			services = append(services, j.newContainer(spec, false, p.Key))
		}
	}
	return job, services, nil
}
