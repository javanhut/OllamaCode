package tools

import "testing"

func TestDefaultRegistryPolicies(t *testing.T) {
	reg := DefaultRegistry()
	for _, tool := range reg.Definitions() {
		if tool.Policy.Modes == 0 {
			t.Errorf("tool %q has no mode policy", tool.Function.Name)
		}
	}

	web, _ := reg.Lookup("web_search")
	if !web.Policy.Allows(ModeExplore) || !web.Policy.SmallModelSafe || !web.Policy.Network {
		t.Fatalf("web_search policy is incomplete: %+v", web.Policy)
	}
	write, _ := reg.Lookup("write_file")
	if write.Policy.Allows(ModeExplore) || !write.Policy.Destructive {
		t.Fatalf("write_file policy is unsafe: %+v", write.Policy)
	}
}

func TestUnknownToolPolicyIsConservative(t *testing.T) {
	p := PolicyForName("external_unclassified")
	if p.Allows(ModeExplore) || p.SmallModelSafe || !p.Destructive || !p.Network {
		t.Fatalf("unexpected external-tool default: %+v", p)
	}
}
