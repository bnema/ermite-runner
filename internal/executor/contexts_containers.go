// SPDX-License-Identifier: MIT

package executor

import "bnema.dev/ermite-actions/expr"

// The job.container and job.services contexts (JobContext.cs:20-47,
// ContainerOperationProvider.cs:96, :215-232, :237-240). The runner's
// JobContext.Container and .Services create their dictionary when they are
// read, which is why a recorded job that ran a docker action has
// "container": {} (act-docker-image).

// jobSub returns the dictionary at job.<key>, creating it when missing.
func (s *State) jobSub(key string) *expr.Dictionary {
	if v, ok := s.job.Get(key); ok {
		if d, ok := v.(*expr.Dictionary); ok {
			return d
		}
	}
	d := expr.NewDictionary()
	s.job = setKey(s.job, key, d)
	return d
}

// TouchJobContainer is the read of JobContext.Container: job.container exists
// from then on.
func (s *State) TouchJobContainer() { s.jobSub("container") }

// SetJobContainer sets job.container.<key> ("network", "id").
func (s *State) SetJobContainer(key, value string) {
	d := s.jobSub("container")
	if _, ok := d.Get(key); ok {
		s.job = setKey(s.job, "container", replaceKey(d, key, expr.String(value)))
		return
	}
	d.Add(key, expr.String(value))
}

// JobContainerNetwork is job.container.network, "" when it is not set.
func (s *State) JobContainerNetwork() string {
	if v, ok := s.jobSub("container").Get("network"); ok {
		return expr.CoerceString(v)
	}
	return ""
}

// SetJobService sets job.services.<alias> ({id, ports, network}).
func (s *State) SetJobService(alias, id, network string, ports []portMapping) {
	svc := expr.NewDictionary()
	svc.Add("id", expr.String(id))
	pd := expr.NewDictionary()
	for _, p := range ports {
		pd.Add(p.container, expr.String(p.host))
	}
	svc.Add("ports", pd)
	svc.Add("network", expr.String(network))
	d := s.jobSub("services")
	s.job = setKey(s.job, "services", replaceKey(d, alias, svc))
}

func replaceKey(d *expr.Dictionary, key string, v expr.Value) *expr.Dictionary {
	return setKey(d, key, v)
}
