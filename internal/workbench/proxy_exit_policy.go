package workbench

import (
	"github.com/sylvinus/agent-vm/internal/cage"
	"github.com/sylvinus/agent-vm/internal/managedproxy"
)

// config.json owns the active policy; the node manager stores inactive policies
// for restoration on selection. Always snapshot the latest active settings.
func selectProxyExitPolicy(next *managedproxy.Store, previous managedproxy.Store, active cage.ExitPolicy, explicit bool) cage.ExitPolicy {
	policies := make(map[string]cage.ExitPolicy, len(next.ExitPolicies)+1)
	for id, policy := range next.ExitPolicies {
		policies[id] = policy
	}
	next.ExitPolicies = policies
	_, owned := previous.ExitPolicies[previous.Selected]
	if owned {
		policies[previous.Selected] = active
	}
	if !explicit {
		return active
	}
	if previous.Selected == next.Selected && owned {
		return active
	}
	policy, exists := policies[next.Selected]
	if !exists {
		// Legacy global anchors cannot be assigned to a node whose identity was
		// never recorded. Keep the selected mode and scope for its first binding.
		policy = cage.ExitPolicy{Mode: active.Mode, Scope: active.Scope}.Normalized()
	}
	policies[next.Selected] = policy
	return policy
}

// Save a user-confirmed or first-use binding with the applied final exit. If the
// node store cannot be saved, restore the active configuration as well.
func (s *Server) saveExitPolicyConfig(cfg cage.Config) error {
	old, err := loadConfig(s.configPath)
	if err != nil {
		return err
	}
	p, err := managedproxy.Applied(s.profile, cfg.ProxyURL, cfg.ProxyRevision)
	if err != nil {
		return err
	}
	if p.ExitPolicies == nil {
		p.ExitPolicies = map[string]cage.ExitPolicy{}
	}
	p.ExitPolicies[p.Selected] = cfg.ExitPolicy
	if err := writePrivate(s.configPath, cfg); err != nil {
		return err
	}
	if err := p.Save(s.profile); err != nil {
		_ = writePrivate(s.configPath, old)
		return err
	}
	return nil
}
