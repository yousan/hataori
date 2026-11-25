package control

// What a FilesRequest does.
const (
	FilesList  = "list"
	FilesRead  = "read"
	FilesWrite = "write"
	FilesEdit  = "edit"
)

// FilesRequest reads or writes a file of the directory of the caller's
// scope's supervisor (API.Files). It names neither the scope nor a thread:
// they are the connection's.
type FilesRequest struct {
	Op string `json:"op"` // FilesList, FilesRead, FilesWrite or FilesEdit
	// Path is the file's, relative to the directory, with "/" between its
	// parts; none for a list.
	Path string `json:"path,omitempty"`
	// Text is the whole text of a write; nil is no write, "" an empty file.
	Text *string `json:"text,omitempty"`
	// Version is, for a write, that of the file it replaces, as the caller
	// last saw it; "" to make a new file.
	Version string `json:"version,omitempty"`
	// Old is the passage that an edit replaces, which must stand exactly
	// once in the file, and New what replaces it; nil is no edit, "" takes
	// the passage out.
	Old string  `json:"old,omitempty"`
	New *string `json:"new,omitempty"`
}

// FilesAnswer is the answer to a FilesRequest.
type FilesAnswer struct {
	// Files are all files of the directory, by their paths, without their
	// texts, for a list.
	Files []ScopeFile `json:"files,omitempty"`
	// File is the file as it is now: for a read and a refusal with its text,
	// after a write or an edit without it. Absent when there is none.
	File *ScopeFile `json:"file,omitempty"`
	// Refused says why a write or an edit did not fit what is there now, as
	// File says it: then it changed nothing. Empty when it was made.
	Refused string `json:"refused,omitempty"`
}

// ScopeFile is a file of a supervisor's directory.
type ScopeFile struct {
	Path string `json:"path"`
	Size int64  `json:"size"` // in bytes
	// Version is a hash of the content; absent for a file that Files does
	// not read: one too large, or one that is not UTF-8.
	Version string `json:"version,omitempty"`
	Text    string `json:"text,omitempty"` // the content, where the answer has it
}
