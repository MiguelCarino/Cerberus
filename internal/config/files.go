package config

// FilesConfig controls files dropped into session workspaces (day phase 2).
type FilesConfig struct {
	UploadDir   string   `json:"upload_dir"`    // folder inside the workspace that receives uploads (default "inbox")
	MaxFileMiB  int      `json:"max_file_mib"`  // largest single upload (default 200)
	MaxTotalMiB int      `json:"max_total_mib"` // most uploaded bytes per session (default 2048)
	AllowExec   bool     `json:"allow_exec"`    // keep the exec bit on uploads (default false: never set)
	DenyExt     []string `json:"deny_ext"`      // refused extensions, e.g. ".exe" (default none)
	KeepNames   bool     `json:"keep_names"`    // keep sanitised original names (default true)
}

func filesDefaults(c *Config) {
	c.Files = FilesConfig{UploadDir: "inbox", MaxFileMiB: 200, MaxTotalMiB: 2048, KeepNames: true}
}
