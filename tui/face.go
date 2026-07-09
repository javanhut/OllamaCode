package tui

import (
	"strings"
	"time"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
	"github.com/javanhut/ollama_code/api"
)

// overlayFace paints the animated mascot box on top of the base view, pinned to
// the bottom-right corner of the transcript — just above the input band and left
// of the sidebar, so it covers neither the prompt nor the panel. It sits ON TOP
// of the chat (an overlay), not behind it.
// faceOn reports whether the mascot overlay is enabled (default off).
func (m *Model) faceOn() bool {
	return m.cfg.Face != nil && *m.cfg.Face
}

// welcomeOn reports whether the empty-chat welcome panel is shown (default off).
func (m *Model) welcomeOn() bool {
	return m.cfg.Welcome != nil && *m.cfg.Welcome
}

func (m *Model) overlayFace(base string) string {
	if !m.faceOn() || m.width <= 0 || m.height <= 0 {
		return base
	}
	face := m.faceView()
	fw := lipgloss.Width(face)
	fh := lipgloss.Height(face)
	inputH := lipgloss.Height(m.inputView())
	col := m.width - m.sidebarSpace() - fw - 1
	row := m.height - inputH - fh
	if col < 0 {
		col = 0
	}
	if row < 0 {
		row = 0
	}
	return overlay(base, face, col, row)
}

// currentFaceMood returns the mascot's mood, recomputing the keyword scan only
// when the conversation actually grows — not on every 400ms animation frame.
func (m *Model) currentFaceMood() faceMood {
	if len(m.history) != m.faceMoodLen {
		m.faceMoodCache = inferFaceMood(m.history)
		m.faceMoodLen = len(m.history)
	}
	return m.faceMoodCache
}

func inferFaceMood(history []api.Message) faceMood {
	type moodScore struct {
		mood  faceMood
		score int
	}
	scores := map[faceMood]int{}
	seen := 0
	for i := len(history) - 1; i >= 0 && seen < 6; i-- {
		msg := history[i]
		if msg.Role != "user" && msg.Role != "assistant" {
			continue
		}
		text := strings.ToLower(msg.Content)
		if strings.TrimSpace(text) == "" {
			continue
		}
		seen++
		weight := 7 - seen
		if msg.Role == "user" {
			weight += 2
		}
		for mood, words := range faceMoodKeywords {
			hits := keywordHits(text, words)
			if hits > 0 {
				scores[mood] += hits * weight
			}
		}
	}
	if len(scores) == 0 {
		return faceMoodNeutral
	}
	ranked := []moodScore{
		{faceMoodFrustrated, scores[faceMoodFrustrated]},
		{faceMoodConfused, scores[faceMoodConfused]},
		{faceMoodConcerned, scores[faceMoodConcerned]},
		{faceMoodSurprised, scores[faceMoodSurprised]},
		{faceMoodHappy, scores[faceMoodHappy]},
		{faceMoodFocused, scores[faceMoodFocused]},
	}
	best := ranked[0]
	for _, s := range ranked[1:] {
		if s.score > best.score {
			best = s
		}
	}
	if best.score < 5 {
		return faceMoodNeutral
	}
	return best.mood
}

func keywordHits(text string, keywords []string) int {
	hits := 0
	for _, kw := range keywords {
		if strings.Contains(text, kw) {
			hits++
		}
	}
	return hits
}

func (m *Model) nextFaceTick() tea.Cmd {
	return tea.Tick(400*time.Millisecond, func(t time.Time) tea.Msg {
		return faceTickMsg(t)
	})
}

// pupilCell returns the 3-cell interior of one eye with the pupil placed
// left, center, or right (see face.txt: |0  |, | 0 |, |  0|).
func pupilCell(dir eyeDir) string {
	switch dir {
	case eyeLeft:
		return "0  "
	case eyeRight:
		return "  0"
	default:
		return " 0 "
	}
}

// renderEyes returns 4 lines (each 12 cells wide) of ASCII eyes, following the
// design in face.txt: a pair of boxed eyes whose pupils look left/center/right,
// with open, half-lidded (sleepy/indifferent), or closed (blink/sleep) lids.
func renderEyes(dir eyeDir, lid eyeLid) []string {
	blank := strings.Repeat(" ", 12)
	pair := func(eye string) string { return eye + "  " + eye }
	switch lid {
	case lidClosed:
		return []string{blank, pair(" --- "), pair(" --- "), blank}
	case lidHalf:
		return []string{pair(" ___ "), pair("|" + pupilCell(dir) + "|"), pair(" --- "), blank}
	default: // open
		return []string{pair(" --- "), pair("|   |"), pair("|" + pupilCell(dir) + "|"), pair(" --- ")}
	}
}

// centerCells pads s to exactly w cells, centered.
func centerCells(s string, w int) string {
	return lipgloss.NewStyle().Width(w).Align(lipgloss.Center).Render(s)
}

// inputGazeDir maps the cursor's horizontal position in the input bar to an eye
// direction, so the mascot's eyes follow along as you type: left of the bar ->
// look left, middle -> center, right -> look right.
func (m *Model) inputGazeDir() eyeDir {
	w := m.input.Width()
	if w <= 0 {
		return eyeCenter
	}
	rel := float64(m.input.LineInfo().CharOffset) / float64(w)
	switch {
	case rel < 0.34:
		return eyeLeft
	case rel > 0.66:
		return eyeRight
	default:
		return eyeCenter
	}
}

func (m *Model) faceView() string {
	var dir eyeDir
	var lid eyeLid
	var mouth string
	var label string

	f := m.faceFrame
	switch {
	case strings.TrimSpace(m.modelName) == "": // no model loaded — asleep
		label = "sleeping..."
		dir, lid = eyeCenter, lidClosed
		z := []string{"z    ", " zZ  ", "  zZ ", "   zZ"}
		mouth = z[(f/2)%len(z)] // slow drift
	case m.streaming: // responding — mouth opens and closes while talking
		label = "speaking"
		dir, lid = eyeCenter, lidOpen
		talk := []string{"-----", "[ - ]", "[   ]", "[ - ]"}
		mouth = talk[f%len(talk)]
	case m.pending != nil: // working — focused on the task
		label = "focused"
		lid = lidOpen
		glance := []eyeDir{eyeLeft, eyeCenter, eyeRight, eyeCenter}
		dir = glance[(f/3)%len(glance)] // slow, deliberate look
		mouth = " --- "
	default: // model loaded & idle — active, expression reflects recent activity
		label, mouth = faceMoodFrame(m.currentFaceMood(), f)
		lid = lidOpen
		if strings.TrimSpace(m.input.Value()) != "" {
			// Follow the cursor: eyes track where you are in the input bar.
			dir = m.inputGazeDir()
		} else {
			// Idle: mostly still, a brief glance per ~7s cycle.
			switch f % 16 {
			case 4, 5:
				dir = eyeLeft
			case 11, 12:
				dir = eyeRight
			default:
				dir = eyeCenter
			}
		}
		if f%16 == 8 {
			lid = lidClosed // single slow blink
		}
	}

	lines := renderEyes(dir, lid)
	lines = append(lines, "", centerCells(mouth, 12))
	art := strings.Join(lines, "\n")

	labelLine := lipgloss.NewStyle().Faint(true).Width(12).Align(lipgloss.Center).Render(label)

	boxStyle := lipgloss.NewStyle().
		Border(lipgloss.RoundedBorder()).
		BorderForeground(m.mode.color()).
		Padding(0, 1).
		Align(lipgloss.Center)
	return boxStyle.Render(art + "\n" + labelLine)
}

// faceMoodFrame maps a conversation mood to the mascot's (label, mouth). The
// eyes are rendered separately by renderEyes; the mood is expressed through the
// mouth shape. frame is accepted for call-site symmetry but moods are static.
func faceMoodFrame(mood faceMood, frame int) (string, string) {
	switch mood {
	case faceMoodHappy:
		return "pleased", "\\___/"
	case faceMoodConcerned:
		return "concerned", " ___ "
	case faceMoodFrustrated:
		return "frustrated", "/^^^\\"
	case faceMoodConfused:
		return "puzzled", " ~?~ "
	case faceMoodSurprised:
		return "surprised", "  O  "
	case faceMoodFocused:
		return "focused", " --- "
	default:
		return "active", " --- "
	}
}
