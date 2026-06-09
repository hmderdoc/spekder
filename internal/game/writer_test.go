package game

import "testing"

// TestMapJSONRoundTrip: MapJSON (Map -> JSON) is the inverse of ParseMapJSON, so
// the editor can save a map and reload it unchanged - including ramps and every
// trait.
func TestMapJSONRoundTrip(t *testing.T) {
	m := Map{
		Version: 1, Name: "RT", Size: 18,
		Obstacles: []Box{{Pos: V3{X: 1, Y: 1, Z: 2}, Half: V3{X: 2, Y: 1, Z: 2}, Color: [3]float64{0.4, 0.4, 0.5}}},
		Ramps:     []Ramp{{Pos: V3{X: -5, Z: 0}, Half: V3{X: 2, Z: 3}, H: 3, Dir: 2, Color: [3]float64{0.3, 0.4, 0.5}}},
		Scenery:   []Prop{{Kind: "obelisk", Pos: V3{}, H: 7, Color: [3]float64{0.8, 0.3, 0.6}}},
		Spawns:    []V3{{X: -14, Z: -14}, {X: 14, Z: 14}},
		Pickups:   []MapPickup{{Pos: V3{X: 0, Z: 10}, Kind: PickAmmo}, {Pos: V3{X: 2, Z: 2}, Kind: PickWeapon, Weapon: 6}},
		Rules:     &MapRules{Mode: int(ModeCTF), TimeLimit: 120, Target: 5, Lives: 3},
		Entities: []Entity{
			{Kind: "turret", Pos: V3{Y: 2}, Half: V3{X: 0.7, Y: 0.3, Z: 0.7}, Solid: true, Weapon: 1,
				Turret:   &TurretTrait{Range: 22, FireDelay: 1.4, Dmg: 16, TurnRate: 1.6},
				Destruct: &DestructTrait{MaxHP: 60}, Respawn: &RespawnTrait{Delay: 12}},
			{Kind: "trampoline", Pos: V3{X: 8, Y: 0.2, Z: 0}, Half: V3{X: 1.5, Y: 0.2, Z: 1.5}, Bounce: &BounceTrait{Power: 13}},
			{Kind: "flag", Pos: V3{X: 0, Z: -12}, Half: V3{X: 0.5, Y: 0.5, Z: 0.5}, Flag: &FlagTrait{Team: 0}},
			{Kind: "zone", Pos: V3{}, Half: V3{X: 4, Y: 1, Z: 4}, Zone: &ZoneTrait{Capture: 5}},
		},
	}
	data, err := MapJSON(m)
	if err != nil {
		t.Fatalf("MapJSON: %v", err)
	}
	got, err := ParseMapJSON(data)
	if err != nil {
		t.Fatalf("ParseMapJSON: %v", err)
	}
	if got.Name != m.Name || got.Size != m.Size {
		t.Fatalf("header lost: %+v", got)
	}
	if len(got.Obstacles) != 1 || got.Obstacles[0].Half.X != 2 {
		t.Fatalf("obstacle lost: %+v", got.Obstacles)
	}
	if len(got.Ramps) != 1 || got.Ramps[0].Dir != 2 || got.Ramps[0].H != 3 {
		t.Fatalf("ramp lost: %+v", got.Ramps)
	}
	if len(got.Spawns) != 2 || got.Spawns[1].Z != 14 || len(got.Pickups) != 2 {
		t.Fatalf("spawns/pickups lost: %+v %+v", got.Spawns, got.Pickups)
	}
	if got.Pickups[0].Kind != PickAmmo || got.Pickups[1].Kind != PickWeapon || got.Pickups[1].Weapon != 6 {
		t.Fatalf("typed pickups lost: %+v", got.Pickups)
	}
	if len(got.Entities) != 4 {
		t.Fatalf("want 4 entities, got %d", len(got.Entities))
	}
	e := got.Entities[0]
	if e.Turret == nil || e.Turret.Dmg != 16 || e.Destruct == nil || e.Destruct.MaxHP != 60 || e.Respawn == nil {
		t.Fatalf("turret entity traits lost: %+v", e)
	}
	if e.Weapon != 1 {
		t.Fatalf("turret weapon lost: %d want 1", e.Weapon)
	}
	if got.Entities[1].Bounce == nil || got.Entities[1].Bounce.Power != 13 {
		t.Fatalf("bounce lost: %+v", got.Entities[1])
	}
	if got.Entities[2].Flag == nil || got.Entities[2].Flag.Team != 0 {
		t.Fatalf("flag lost: %+v", got.Entities[2])
	}
	if got.Entities[3].Zone == nil || got.Entities[3].Zone.Capture != 5 {
		t.Fatalf("zone lost: %+v", got.Entities[3])
	}
	if got.Rules == nil || got.Rules.Mode != int(ModeCTF) || got.Rules.TimeLimit != 120 || got.Rules.Target != 5 || got.Rules.Lives != 3 {
		t.Fatalf("rules lost: %+v", got.Rules)
	}
	if len(ValidateMap(got)) != 0 {
		t.Fatalf("round-tripped map should validate clean: %v", ValidateMap(got))
	}
}
