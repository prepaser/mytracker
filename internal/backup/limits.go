package backup

const (
	maxBackupUncompressedSize int64 = 8 << 30
	maxBackupDatabaseSize     int64 = 4 << 30
	maxBackupProofSize        int64 = 64 << 20
	maxBackupManifestSize     int64 = maxBackupUncompressedSize
)
