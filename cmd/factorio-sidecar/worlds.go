// Copyright 2026 Google LLC
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package main

import (
	"encoding/json"
	"fmt"
)

// WorldSpec defines the per-Actor server name, description, map generation
// preset, custom map-gen-settings.json overrides, and initial RCON Lua
// setup executed when a new world save is generated on first connect.
type WorldSpec struct {
	ActorName      string
	ServerName     string
	Description    string
	Tags           []string
	MapGenPreset   string
	MapGenSettings map[string]any
	InitLua        string
}

// GetWorldSpec returns the custom Factorio world configuration for the given
// Substrate Actor name.
func GetWorldSpec(actorName string) WorldSpec {
	switch actorName {
	case "world-vanilla":
		return WorldSpec{
			ActorName:    "world-vanilla",
			ServerName:   "[Substrate #1] Super-Dense Megabase (600% Rich Ores)",
			Description:  "Peaceful megabase paradise on Agent Substrate: 600% resource richness, huge starting area, zero cliffs, and a starter chest with Power Armor & Construction Bots.",
			Tags:         []string{"substrate", "megabase", "rich-resources", "peaceful"},
			MapGenPreset: "rich-resources",
			MapGenSettings: map[string]any{
				"peaceful_mode": true,
				"starting_area": 3,
				"cliff_settings": map[string]any{
					"name":                     "cliff",
					"cliff_elevation_0":        10,
					"cliff_elevation_interval": 40,
					"richness":                 0,
				},
				"autoplace_controls": map[string]any{
					"iron-ore":    map[string]any{"frequency": 2, "size": 4, "richness": 6},
					"copper-ore":  map[string]any{"frequency": 2, "size": 4, "richness": 6},
					"coal":        map[string]any{"frequency": 2, "size": 4, "richness": 6},
					"stone":       map[string]any{"frequency": 2, "size": 4, "richness": 6},
					"crude-oil":   map[string]any{"frequency": 3, "size": 4, "richness": 6},
					"uranium-ore": map[string]any{"frequency": 2, "size": 4, "richness": 6},
					"trees":       map[string]any{"frequency": 0.5, "size": 0.5, "richness": 1},
				},
			},
			InitLua: `local s = game.surfaces[1]; ` +
				`game.forces.player.chart(s, {{-160, -160}, {160, 160}}); ` +
				`game.forces.player.add_chart_tag(s, {position={0, -4}, text="[Substrate #1] Super-Dense Megabase (600% Ores)"}); ` +
				`local p = s.find_non_colliding_position("steel-chest", {2, 2}, 16, 1) or {2, 2}; ` +
				`local c = s.create_entity{name="steel-chest", position=p, force="player"}; ` +
				`if c then ` +
				`c.insert{name="power-armor", count=1}; ` +
				`c.insert{name="fusion-reactor-equipment", count=1}; ` +
				`c.insert{name="exoskeleton-equipment", count=2}; ` +
				`c.insert{name="personal-roboport-equipment", count=2}; ` +
				`c.insert{name="construction-robot", count=20}; ` +
				`c.insert{name="express-transport-belt", count=200}; ` +
				`c.insert{name="electric-mining-drill", count=50}; ` +
				`c.insert{name="medium-electric-pole", count=50}; ` +
				`c.insert{name="solar-panel", count=50}; ` +
				`end`,
		}

	case "world-deathworld":
		return WorldSpec{
			ActorName:    "world-deathworld",
			ServerName:   "[Substrate #2] Deathworld Biter Swarm (400% Enemy Bases)",
			Description:  "High-intensity Deathworld on Agent Substrate: 400% biter nest frequency & size surrounding a fortified spawn bunker with turrets, walls, and 400% rich ores.",
			Tags:         []string{"substrate", "deathworld", "biters", "survival"},
			MapGenPreset: "death-world",
			MapGenSettings: map[string]any{
				"peaceful_mode": false,
				"starting_area": 0.75,
				"autoplace_controls": map[string]any{
					"enemy-base":  map[string]any{"frequency": 4, "size": 4, "richness": 4},
					"iron-ore":    map[string]any{"frequency": 2, "size": 3, "richness": 4},
					"copper-ore":  map[string]any{"frequency": 2, "size": 3, "richness": 4},
					"coal":        map[string]any{"frequency": 2, "size": 3, "richness": 4},
					"stone":       map[string]any{"frequency": 2, "size": 3, "richness": 4},
					"crude-oil":   map[string]any{"frequency": 3, "size": 3, "richness": 4},
					"uranium-ore": map[string]any{"frequency": 3, "size": 3, "richness": 6},
				},
			},
			InitLua: `local s = game.surfaces[1]; ` +
				`game.forces.player.chart(s, {{-160, -160}, {160, 160}}); ` +
				`game.forces.player.add_chart_tag(s, {position={0, -4}, text="[Substrate #2] Deathworld Bunker (400% Biters)"}); ` +
				`for _, pos in ipairs({{-5, -5}, {5, -5}, {-5, 5}, {5, 5}}) do ` +
				`local tp = s.find_non_colliding_position("gun-turret", pos, 8, 1); ` +
				`if tp then local t = s.create_entity{name="gun-turret", position=tp, force="player"}; if t then t.insert{name="piercing-rounds-magazine", count=50} end end ` +
				`end; ` +
				`local p = s.find_non_colliding_position("steel-chest", {2, 2}, 16, 1) or {2, 2}; ` +
				`local c = s.create_entity{name="steel-chest", position=p, force="player"}; ` +
				`if c then ` +
				`c.insert{name="heavy-armor", count=1}; ` +
				`c.insert{name="submachine-gun", count=1}; ` +
				`c.insert{name="piercing-rounds-magazine", count=400}; ` +
				`c.insert{name="gun-turret", count=20}; ` +
				`c.insert{name="stone-wall", count=200}; ` +
				`c.insert{name="repair-pack", count=50}; ` +
				`c.insert{name="grenade", count=50}; ` +
				`end`,
		}

	case "world-railworld":
		return WorldSpec{
			ActorName:    "world-railworld",
			ServerName:   "[Substrate #3] Rail World Express (Giant Outpost Patches)",
			Description:  "Expansive Rail World on Agent Substrate: massive 600% size & richness distant ore patches, no biter expansion, no cliffs, and a full locomotive & rail starter depot.",
			Tags:         []string{"substrate", "railworld", "trains", "outposts"},
			MapGenPreset: "rail-world",
			MapGenSettings: map[string]any{
				"peaceful_mode": false,
				"starting_area": 2,
				"cliff_settings": map[string]any{
					"name":                     "cliff",
					"cliff_elevation_0":        10,
					"cliff_elevation_interval": 40,
					"richness":                 0,
				},
				"autoplace_controls": map[string]any{
					"iron-ore":    map[string]any{"frequency": 0.25, "size": 6, "richness": 6},
					"copper-ore":  map[string]any{"frequency": 0.25, "size": 6, "richness": 6},
					"coal":        map[string]any{"frequency": 0.25, "size": 6, "richness": 6},
					"stone":       map[string]any{"frequency": 0.25, "size": 6, "richness": 6},
					"crude-oil":   map[string]any{"frequency": 0.33, "size": 6, "richness": 6},
					"uranium-ore": map[string]any{"frequency": 0.25, "size": 6, "richness": 6},
					"water":       map[string]any{"frequency": 0.5, "size": 1.5},
				},
			},
			InitLua: `local s = game.surfaces[1]; ` +
				`game.forces.player.chart(s, {{-224, -224}, {224, 224}}); ` +
				`game.forces.player.add_chart_tag(s, {position={0, -4}, text="[Substrate #3] Rail World Depot (600% Giant Outposts)"}); ` +
				`local p = s.find_non_colliding_position("steel-chest", {2, 2}, 16, 1) or {2, 2}; ` +
				`local c = s.create_entity{name="steel-chest", position=p, force="player"}; ` +
				`if c then ` +
				`c.insert{name="rail", count=1000}; ` +
				`c.insert{name="locomotive", count=5}; ` +
				`c.insert{name="cargo-wagon", count=10}; ` +
				`c.insert{name="train-stop", count=10}; ` +
				`c.insert{name="rail-signal", count=50}; ` +
				`c.insert{name="rail-chain-signal", count=50}; ` +
				`c.insert{name="big-electric-pole", count=50}; ` +
				`c.insert{name="rocket-fuel", count=60}; ` +
				`c.insert{name="fast-inserter", count=50}; ` +
				`end`,
		}

	case "world-ribbonworld":
		return WorldSpec{
			ActorName:    "world-ribbonworld",
			ServerName:   "[Substrate #4] Ribbon World 64-Tile Strip (600% Rich)",
			Description:  "Narrow 64-tile tall horizontal Ribbon World on Agent Substrate: visible void edges at top & bottom of screen, 600% rich ore veins, and landfill/belt starter chest.",
			Tags:         []string{"substrate", "ribbonworld", "challenge", "rich-resources"},
			MapGenPreset: "ribbon-world",
			MapGenSettings: map[string]any{
				"width":         0,
				"height":        64,
				"starting_area": 2,
				"cliff_settings": map[string]any{
					"name":                     "cliff",
					"cliff_elevation_0":        10,
					"cliff_elevation_interval": 40,
					"richness":                 0,
				},
				"autoplace_controls": map[string]any{
					"iron-ore":    map[string]any{"frequency": 3, "size": 3, "richness": 6},
					"copper-ore":  map[string]any{"frequency": 3, "size": 3, "richness": 6},
					"coal":        map[string]any{"frequency": 3, "size": 3, "richness": 6},
					"stone":       map[string]any{"frequency": 3, "size": 3, "richness": 6},
					"crude-oil":   map[string]any{"frequency": 3, "size": 3, "richness": 6},
					"uranium-ore": map[string]any{"frequency": 3, "size": 3, "richness": 6},
					"enemy-base":  map[string]any{"frequency": 2, "size": 2, "richness": 2},
				},
			},
			InitLua: `local s = game.surfaces[1]; ` +
				`game.forces.player.chart(s, {{-320, -32}, {320, 32}}); ` +
				`game.forces.player.add_chart_tag(s, {position={0, -4}, text="[Substrate #4] 64-Tile Ribbon World (600% Rich Strip)"}); ` +
				`local p = s.find_non_colliding_position("steel-chest", {2, 2}, 16, 1) or {2, 2}; ` +
				`local c = s.create_entity{name="steel-chest", position=p, force="player"}; ` +
				`if c then ` +
				`c.insert{name="landfill", count=500}; ` +
				`c.insert{name="fast-transport-belt", count=200}; ` +
				`c.insert{name="fast-underground-belt", count=50}; ` +
				`c.insert{name="fast-splitter", count=50}; ` +
				`c.insert{name="assembling-machine-2", count=50}; ` +
				`c.insert{name="electric-mining-drill", count=50}; ` +
				`c.insert{name="fast-inserter", count=50}; ` +
				`end`,
		}

	default:
		if actorName == "" {
			actorName = "golden"
		}
		return WorldSpec{
			ActorName:      actorName,
			ServerName:     fmt.Sprintf("[Substrate] Factorio World (%s)", actorName),
			Description:    fmt.Sprintf("On-demand Factorio server hosted as Agent Substrate Actor %q", actorName),
			Tags:           []string{"substrate", "gke", "factorio", actorName},
			MapGenPreset:   "rich-resources",
			MapGenSettings: map[string]any{},
		}
	}
}

// ServerSettingsJSON builds the Factorio server-settings.json document for this world.
func (w WorldSpec) ServerSettingsJSON(requireUserVerification bool) ([]byte, error) {
	settings := map[string]any{
		"name":        w.ServerName,
		"description": w.Description,
		"tags":        w.Tags,
		"max_players": 0,
		"visibility": map[string]bool{
			"public": false,
			"lan":    false,
		},
		"username":                                "",
		"password":                                "",
		"token":                                   "",
		"game_password":                           "",
		"require_user_verification":               requireUserVerification,
		"max_upload_in_kilobytes_per_second":      0,
		"max_upload_slots":                        5,
		"minimum_latency_in_ticks":                0,
		"max_heartbeats_per_second":               60,
		"ignore_player_limit_for_returning_players": false,
		"allow_commands":                          "admins-only",
		"autosave_interval":                       5,
		"autosave_slots":                          3,
		"afk_autokick_interval":                   0,
		"auto_pause":                              true,
		"auto_pause_when_players_connect":         false,
		"only_admins_can_pause_the_game":          true,
		"autosave_only_on_server":                 true,
		"non_blocking_saving":                     false,
		"minimum_segment_size":                    25,
		"minimum_segment_size_peer_count":         20,
		"maximum_segment_size":                    100,
		"maximum_segment_size_peer_count":         10,
	}
	return json.MarshalIndent(settings, "", "  ")
}

// MapGenSettingsJSON builds the Factorio map-gen-settings.json document for this world.
func (w WorldSpec) MapGenSettingsJSON() ([]byte, error) {
	settings := w.MapGenSettings
	if settings == nil {
		settings = map[string]any{}
	}
	return json.MarshalIndent(settings, "", "  ")
}
