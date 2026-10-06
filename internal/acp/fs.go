package acp

import (
	"context"
	"errors"
	"path/filepath"
)

// Reading files through the editor, and why writing them does not go back the
// same way.
//
// fs/read_text_file is taken up. The client answers from the buffer it has
// open, so a file the user has edited and not saved comes back as they see it.
// That is a correctness fix, not a nicety: Klaudia would otherwise read the
// stale text on disk, explain something that is no longer there, and — worse —
// base an Edit on an old_string the user had already changed.
//
// fs/write_text_file is declined. Klaudia's Write and Edit are not "put these
// bytes on disk":
//
//   - Edit is a read-modify-write with a uniqueness check on old_string and a
//     staleness check against what was read. Taking the read from the editor's
//     buffer and handing the write to the editor would be consistent; taking
//     the read from the editor and the write from the same place while the
//     *mtime* check still looks at disk would not, and the check exists to
//     catch exactly the case where two writers disagree.
//   - Every mutating path goes through the trust gate, the permission prompt and
//     BeforeEdit's undo checkpoint, all of which are expressed in terms of a
//     path on disk. /undo restores a file; it cannot restore an editor buffer.
//
// So Klaudia writes to disk and lets the editor notice, which is what it does
// for every other tool that changes a file (git checkout from Bash, a formatter,
// a code generator). The asymmetry is the point: reading someone else's
// uncommitted view is safe, writing into it is not.

// errNoFSRead is the hook's answer when this client cannot serve files. The
// Read tool falls back to disk on any error, so the text is for Klaudia's own
// logs rather than for the model.
var errNoFSRead = errors.New("client does not serve fs/read_text_file")

// readTextFile backs tools.Context.ReadText for one session.
//
// Non-absolute paths are refused rather than resolved: the spec requires an
// absolute path, and Read has already resolved relative ones against the
// working dir, so anything still relative here is a bug that should fall back
// to disk rather than be guessed at.
func (a *Agent) readTextFile(ctx context.Context, sessionID, path string, line, limit int) (string, error) {
	a.mu.Lock()
	ok := a.caps.FS.ReadTextFile
	a.mu.Unlock()
	if !ok {
		return "", errNoFSRead
	}
	if !filepath.IsAbs(path) {
		return "", errNoFSRead
	}
	p := readTextFileParams{SessionID: sessionID, Path: path}
	if line > 1 {
		p.Line = &line
	}
	if limit > 0 {
		p.Limit = &limit
	}
	var res readTextFileResult
	if err := a.conn.call(ctx, "fs/read_text_file", p, &res); err != nil {
		return "", err
	}
	return res.Content, nil
}
