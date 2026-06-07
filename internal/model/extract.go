package model

// RestoreAction is the typed per-file action restic reports on a restore
// verbose_status line. resticx maps restic's raw English vocabulary
// ("restored", "updated metadata", "skipped") to this enum at the parse
// boundary so presentation layers pick glyphs from structure, never from
// restic's wire strings — if a future restic renames an action, only the
// resticx mapping (and its fixtures) change, not the app or TUI. Mirrors the
// diff feature's ChangeType.
type RestoreAction uint8

const (
	RestoreActionOther    RestoreAction = iota // unrecognized / future action
	RestoreActionRestored                      // "restored"
	RestoreActionMetadata                      // "updated metadata"
	RestoreActionSkipped                       // "skipped"
)
