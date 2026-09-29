package session

import (
	"bytes"
	"encoding/json"
	"io"
	"os"

	"github.com/google/uuid"
)

// clearMarker is the transcript line /clear leaves at the end of the session it
// ends. Read skips it (it is neither "user" nor "assistant"), so an explicit
// resume of the session still sees the whole conversation.
type clearMarker struct {
	Type       string  `json:"type"`    // "system"
	Subtype    string  `json:"subtype"` // "clear"
	UUID       string  `json:"uuid"`
	ParentUUID *string `json:"parentUuid"`
	SessionID  string  `json:"sessionId"`
	Timestamp  string  `json:"timestamp"`
	CWD        string  `json:"cwd"`
	Version    string  `json:"version"`
}

// MarkCleared records that the user cleared this conversation, and reports
// whether the transcript holds anything worth resuming. A transcript that
// never recorded a message is left alone: writing the marker would create a
// file for a session that has nothing in it.
//
// The marker is what keeps auto-resume from reviving a conversation the user
// cleared and then quit without saying anything more: this transcript is still
// the newest in the folder, and EndsCleared tells the launcher to start fresh.
func (t *Transcript) MarkCleared() (bool, error) {
	st, err := os.Stat(t.w.Path())
	if err != nil || st.Size() == 0 {
		return false, nil
	}
	id := uuid.NewString()
	b, err := json.Marshal(clearMarker{
		Type:       "system",
		Subtype:    "clear",
		UUID:       id,
		ParentUUID: t.lastUUID,
		SessionID:  t.meta.SessionID,
		Timestamp:  Now(),
		CWD:        t.meta.CWD,
		Version:    t.meta.Version,
	})
	if err != nil {
		return true, err
	}
	if err := t.w.open(); err != nil {
		return true, err
	}
	t.lastUUID = &id
	_, err = t.w.f.Write(append(b, '\n'))
	return true, err
}

// clearTail bounds how much of a transcript EndsCleared reads. A marker is a
// few hundred bytes; a last line longer than this is a message, not a marker.
const clearTail = 64 * 1024

// EndsCleared reports whether the transcript's last line is the marker /clear
// leaves: the user's last act in that session was to clear it. Only the tail of
// the file is read, so a long transcript costs no more than a short one.
func EndsCleared(path string) bool {
	f, err := os.Open(path)
	if err != nil {
		return false
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return false
	}
	off := st.Size() - clearTail
	if off < 0 {
		off = 0
	}
	buf, err := io.ReadAll(io.NewSectionReader(f, off, st.Size()-off))
	if err != nil {
		return false
	}
	buf = bytes.TrimRight(buf, "\r\n")
	if i := bytes.LastIndexByte(buf, '\n'); i >= 0 {
		buf = buf[i+1:]
	}
	var e struct {
		Type    string `json:"type"`
		Subtype string `json:"subtype"`
	}
	return json.Unmarshal(buf, &e) == nil && e.Type == "system" && e.Subtype == "clear"
}
