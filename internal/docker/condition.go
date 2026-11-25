package docker

import (
	"maps"
	"slices"
)

// Condition is the overall state of a stack, derived from its services and
// containers (Overview.Condition). The stack list shows it (issue #23),
// the notification summary reports it (issue #48).
type Condition string

// Stack conditions.
const (
	ConditionStarted    Condition = "started"     // every service runs (healthy, if it has a healthcheck)
	ConditionStopped    Condition = "stopped"     // no container runs
	ConditionDegraded   Condition = "degraded"    // some but not all services run, or one is unhealthy or restarting
	ConditionNotCreated Condition = "not created" // the stack has no containers
)

// Condition classifies the stack and returns what is wrong with a degraded
// one ("db is not running", "app-1 is unhealthy").
//
// Containers that exited with code 0 while others run are one-shot
// services (init, migrations) and are ignored. A container whose health
// is "starting" counts as running.
func (ov Overview) Condition() (Condition, []string) {
	if len(ov.Containers) == 0 {
		return ConditionNotCreated, nil
	}
	type service struct{ running, done bool }
	services := map[string]*service{}
	for _, name := range ov.Services {
		services[name] = &service{}
	}
	var problems []string
	anyRunning := false
	for _, c := range ov.Containers {
		svc := services[c.Service]
		if svc == nil { // e.g. a service of an inactive profile
			svc = &service{}
			services[c.Service] = svc
		}
		switch {
		case c.State == "running" && c.Health == "unhealthy":
			problems = append(problems, c.Name+" is unhealthy")
			svc.running, anyRunning = true, true
		case c.State == "running":
			svc.running, anyRunning = true, true
		case c.State == "restarting":
			problems = append(problems, c.Name+" is restarting")
		case c.State == "exited" && c.ExitCode == 0:
			svc.done = true
		}
	}
	if !anyRunning && len(problems) == 0 {
		return ConditionStopped, nil
	}
	for _, name := range slices.Sorted(maps.Keys(services)) {
		if svc := services[name]; !svc.running && !svc.done {
			problems = append(problems, name+" is not running")
		}
	}
	if len(problems) > 0 {
		return ConditionDegraded, problems
	}
	return ConditionStarted, nil
}
