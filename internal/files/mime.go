package files

import (
	"mime"
	"path"
	"strings"
)

// mimeTypes is a fixed table so a listing looks the same on every system
// (mime.TypeByExtension reads /etc/mime.types and the Windows registry).
var mimeTypes = map[string]string{
	".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp",
	".bmp": "image/bmp", ".svg": "image/svg+xml", ".ico": "image/x-icon", ".avif": "image/avif", ".heic": "image/heic",
	".tif": "image/tiff", ".tiff": "image/tiff",
	".pdf": "application/pdf", ".zip": "application/zip", ".gz": "application/gzip", ".tar": "application/x-tar",
	".json": "application/json", ".xml": "application/xml", ".wasm": "application/wasm",
	".mp3": "audio/mpeg", ".wav": "audio/wav", ".ogg": "audio/ogg", ".opus": "audio/opus", ".flac": "audio/flac",
	".m4a": "audio/mp4", ".mp4": "video/mp4", ".webm": "video/webm", ".mov": "video/quicktime", ".mkv": "video/x-matroska",
	".html": "text/html", ".htm": "text/html", ".css": "text/css", ".csv": "text/csv", ".md": "text/markdown",
	".txt": "text/plain", ".log": "text/plain", ".js": "text/javascript", ".mjs": "text/javascript",
}

// textExts are source and config files shown as text whatever their MIME
// type says.
var textExts = map[string]bool{
	".go": true, ".py": true, ".rs": true, ".c": true, ".h": true, ".cc": true, ".cpp": true, ".hpp": true,
	".java": true, ".kt": true, ".swift": true, ".rb": true, ".php": true, ".pl": true, ".lua": true, ".sh": true,
	".bash": true, ".zsh": true, ".fish": true, ".ps1": true, ".bat": true, ".ts": true, ".tsx": true, ".jsx": true,
	".vue": true, ".svelte": true, ".sql": true, ".yaml": true, ".yml": true, ".toml": true, ".ini": true,
	".cfg": true, ".conf": true, ".env": true, ".gitignore": true, ".dockerfile": true, ".mk": true, ".cmake": true,
	".gradle": true, ".tex": true, ".rst": true, ".org": true, ".diff": true, ".patch": true, ".srt": true,
	".vtt": true, ".lrc": true, ".tsv": true, ".svg": true, ".json": true, ".xml": true, ".jsonl": true,
	".ndjson": true, ".proto": true, ".graphql": true, ".scss": true, ".less": true, ".dart": true, ".zig": true,
	".nim": true, ".ex": true, ".exs": true, ".erl": true, ".hs": true, ".ml": true, ".clj": true, ".scala": true,
	".r": true, ".jl": true, ".m": true, ".cs": true, ".fs": true, ".vb": true, ".pem": true, ".pub": true,
}

// textNames are well-known files without an extension.
var textNames = map[string]bool{
	"makefile": true, "dockerfile": true, "readme": true, "license": true, "copying": true, "authors": true,
	"changelog": true, "gemfile": true, "rakefile": true, "procfile": true, "justfile": true, "vagrantfile": true,
}

// kindOf returns the MIME type of a file name and whether it previews as
// text or as an image.
func kindOf(name string) (mt string, text, image bool) {
	ext := strings.ToLower(path.Ext(name))
	mt = mimeTypes[ext]
	if mt == "" && ext != "" {
		if t := mime.TypeByExtension(ext); t != "" {
			mt, _, _ = strings.Cut(t, ";")
		}
	}
	if mt == "" && textExts[ext] {
		mt = "text/plain"
	}
	if mt == "" && ext == "" && textNames[strings.ToLower(name)] {
		mt = "text/plain"
	}
	if mt == "" {
		mt = "application/octet-stream"
	}
	text = strings.HasPrefix(mt, "text/") || textExts[ext] || (ext == "" && textNames[strings.ToLower(name)])
	image = strings.HasPrefix(mt, "image/")
	return mt, text, image
}
