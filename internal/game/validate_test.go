package game

import "testing"

func hasField(issues []MapIssue, field string, fatal bool) bool {
	for _, i := range issues {
		if i.Field == field && i.Fatal == fatal {
			return true
		}
	}
	return false
}

// TestValidateCatchesBadMap: the validator flags the fatal authoring mistakes
// (bad trait params, zero footprint) and warns on the soft ones.
func TestValidateCatchesBadMap(t *testing.T) {
	m := Map{
		// no Name -> fatal; no Spawns -> warn
		Size: -1, // fatal
		Entities: []Entity{
			{
				Kind: "turret", Half: V3{X: 0, Y: 1, Z: 1}, // half.X<=0 -> fatal
				Turret:  &TurretTrait{Range: 0},  // range<=0 -> fatal
				Hazard:  &HazardTrait{DPS: 0},    // dps<=0 -> fatal
				Respawn: &RespawnTrait{Delay: 1}, // respawn w/o destruct -> warn
				Bounce:  &BounceTrait{Power: -5}, // power<=0 -> fatal
			},
		},
	}
	issues := ValidateMap(m)
	if !FatalIssues(issues) {
		t.Fatal("expected fatal issues")
	}
	for _, f := range []string{"name", "size", "half", "turret.range", "hazard.dps", "bounce.power"} {
		if !hasField(issues, f, true) {
			t.Errorf("expected fatal issue on %q, got: %v", f, issues)
		}
	}
	if !hasField(issues, "respawn", false) {
		t.Errorf("expected warn on respawn-without-destruct, got: %v", issues)
	}
	if !hasField(issues, "spawns", false) {
		t.Errorf("expected warn on missing spawns, got: %v", issues)
	}
}

// TestValidateCleanMap: a well-formed map produces no issues.
func TestValidateCleanMap(t *testing.T) {
	m := Map{
		Name: "CLEAN", Size: 18,
		Spawns: []V3{{X: -14, Z: -14}, {X: 14, Z: 14}},
		Entities: []Entity{{
			Kind: "turret", Pos: V3{Y: 2}, Half: V3{X: 0.7, Y: 0.3, Z: 0.7},
			Turret:   &TurretTrait{Range: 20, FireDelay: 1.4, Dmg: 14, TurnRate: 1.5},
			Destruct: &DestructTrait{MaxHP: 60}, Respawn: &RespawnTrait{Delay: 12},
		}},
	}
	if issues := ValidateMap(m); len(issues) != 0 {
		t.Fatalf("clean map should validate with no issues, got: %v", issues)
	}
}
