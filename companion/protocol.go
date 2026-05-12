package companion

// Message is the line-delimited JSON envelope used in both directions over
// the companion subprocess's stdin/stdout. The parent CLI and the companion
// agree on a small set of Type values.
//
// Companion -> CLI:
//
//	{"type":"ready"}              emitted once after capture + STT/TTS start
//	{"type":"transcript","text":"..."}  STT result (one utterance)
//	{"type":"error","text":"..."} non-fatal warning the CLI may surface
//
// CLI -> Companion:
//
//	{"type":"speak","text":"..."} ask companion to vocalize text
//	{"type":"stop"}               cancel any in-flight TTS playback
//	{"type":"shutdown"}           graceful exit
type Message struct {
	Type string `json:"type"`
	Text string `json:"text,omitempty"`
}

const (
	MsgReady      = "ready"
	MsgTranscript = "transcript"
	MsgError      = "error"
	MsgSpeak      = "speak"
	MsgStop       = "stop"
	MsgShutdown   = "shutdown"
)
