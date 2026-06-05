package agent

import "sort"

// Insight kinds produced by reflection.
const (
	// KindSkill marks a capability executed often enough to be treated as a
	// learned, reusable skill.
	KindSkill = "skill.frequent_capability"
	// KindFriction marks a capability repeatedly denied — actionable signal
	// that a policy grant may be missing.
	KindFriction = "friction.denied_capability"
)

// Reflection thresholds: a capability becomes a skill once executed at least
// SkillThreshold times; friction is raised once denied at least
// FrictionThreshold times.
const (
	SkillThreshold    = 2
	FrictionThreshold = 2
)

// Observation is a single recorded (capability, status) pair drawn from
// trajectories — the raw material for reflection.
type Observation struct {
	Capability string
	Status     Status
}

// Insight is an advisory conclusion drawn from trajectories. It is strictly
// informational: it feeds planning, never execution. Reflection never acts.
type Insight struct {
	Kind       string
	Capability string
	Count      int
}

// ObservationsFromTrajectories flattens trajectories into observations.
func ObservationsFromTrajectories(trajectories ...Trajectory) []Observation {
	var obs []Observation
	for _, tr := range trajectories {
		for _, o := range tr.Outcomes() {
			obs = append(obs, Observation{Capability: o.Capability, Status: o.Status})
		}
	}
	return obs
}

// Reflect derives skill and friction insights from observations. Pure and
// deterministic: no I/O, no clock. Output is sorted for stable results.
func Reflect(observations []Observation) []Insight {
	executed := map[string]int{}
	denied := map[string]int{}
	for _, o := range observations {
		switch o.Status {
		case StatusExecuted:
			executed[o.Capability]++
		case StatusDenied:
			denied[o.Capability]++
		}
	}

	var insights []Insight
	for capability, n := range executed {
		if n >= SkillThreshold {
			insights = append(insights, Insight{Kind: KindSkill, Capability: capability, Count: n})
		}
	}
	for capability, n := range denied {
		if n >= FrictionThreshold {
			insights = append(insights, Insight{Kind: KindFriction, Capability: capability, Count: n})
		}
	}

	sort.Slice(insights, func(i, j int) bool {
		if insights[i].Kind != insights[j].Kind {
			return insights[i].Kind < insights[j].Kind
		}
		return insights[i].Capability < insights[j].Capability
	})
	return insights
}
