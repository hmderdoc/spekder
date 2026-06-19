package main

import (
	"encoding/json"
	"log"
	"os"
	"sort"

	"spekder/internal/proto"
)

// Global high-score board: per-mode top runs submitted by doors (online matches
// only - solo stays on the caller's machine), persisted to a JSON file so the
// board survives restarts. Alongside it, a per-player career aggregate (keyed by
// Name@BBS) feeds the cumulative / skill-rate / survival-wave boards. See SCORING.md.

const scoresPerMode = 20

func (s *server) loadScores() {
	s.board = map[string][]proto.ScoreRow{}
	if s.scoresPath != "" {
		if data, err := os.ReadFile(s.scoresPath); err == nil {
			_ = json.Unmarshal(data, &s.board)
		}
	}
	s.players = map[string]*proto.PlayerRow{}
	if s.playersPath != "" {
		if data, err := os.ReadFile(s.playersPath); err == nil {
			_ = json.Unmarshal(data, &s.players)
		}
	}
}

// playerKey identifies a player across submissions (the board vouches the handle).
func playerKey(name, bbs string) string { return bbs + "\x00" + name }

// recordScore inserts a submitted score into its mode's table (top-N), folds the
// match into the submitter's career aggregate, and persists both.
func (s *server) recordScore(sub proto.ScoreSubmit) {
	row := proto.ScoreRow{Mode: sub.Mode, Name: sub.Name, BBS: sub.BBS, Map: sub.Map, Score: sub.Score, When: sub.When, Kills: sub.Kills, Caps: sub.Captures}
	s.mu.Lock()
	if s.board == nil {
		s.board = map[string][]proto.ScoreRow{}
	}
	list := append(s.board[sub.Mode], row)
	sort.SliceStable(list, func(i, j int) bool { return list[i].Score > list[j].Score })
	if len(list) > scoresPerMode {
		list = list[:scoresPerMode]
	}
	s.board[sub.Mode] = list

	// Career aggregate (keyed by Name@BBS).
	if s.players == nil {
		s.players = map[string]*proto.PlayerRow{}
	}
	key := playerKey(sub.Name, sub.BBS)
	a := s.players[key]
	if a == nil {
		a = &proto.PlayerRow{Name: sub.Name, BBS: sub.BBS}
		s.players[key] = a
	}
	a.Games++
	if sub.Won {
		a.Wins++
	}
	a.Kills += sub.Kills
	a.Deaths += sub.Deaths
	a.KillsHuman += sub.KillsHuman
	a.KillsBot += sub.KillsBot
	a.Captures += sub.Captures
	a.ShotsFired += sub.ShotsFired
	a.ShotsHit += sub.ShotsHit
	a.TotalScore += sub.Score
	if sub.Score > a.BestScore {
		a.BestScore = sub.Score
	}
	if sub.Wave > a.BestWave {
		a.BestWave = sub.Wave
	}
	// Per-mode record (for the per-mode K/D & win% boards).
	mi := -1
	for k := range a.Modes {
		if a.Modes[k].Mode == sub.Mode {
			mi = k
			break
		}
	}
	if mi < 0 {
		a.Modes = append(a.Modes, proto.ModeAgg{Mode: sub.Mode})
		mi = len(a.Modes) - 1
	}
	a.Modes[mi].Games++
	if sub.Won {
		a.Modes[mi].Wins++
	}
	a.Modes[mi].Kills += sub.Kills
	a.Modes[mi].Deaths += sub.Deaths

	boardData, _ := json.MarshalIndent(s.board, "", "  ")
	playerData, _ := json.MarshalIndent(s.players, "", "  ")
	scoresPath, playersPath := s.scoresPath, s.playersPath
	s.mu.Unlock()

	if scoresPath != "" && boardData != nil {
		_ = os.WriteFile(scoresPath, boardData, 0o644)
	}
	if playersPath != "" && playerData != nil {
		_ = os.WriteFile(playersPath, playerData, 0o644)
	}
	log.Printf("score: %q@%q %s %d (%s)", sub.Name, sub.BBS, sub.Mode, sub.Score, sub.Map)
}

// scoreRows flattens the whole board for a query reply.
func (s *server) scoreRows() []proto.ScoreRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []proto.ScoreRow
	for _, rows := range s.board {
		out = append(out, rows...)
	}
	return out
}

// playerRows flattens the career aggregate for a query reply.
func (s *server) playerRows() []proto.PlayerRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]proto.PlayerRow, 0, len(s.players))
	for _, a := range s.players {
		out = append(out, *a)
	}
	return out
}
