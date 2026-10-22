package drive

import "time"

type Token struct {
	AccessToken  string    `json:"access_token"`
	RefreshToken string    `json:"refresh_token"`
	ExpiredAt    time.Time `json:"expired_at"`
	DriveID      string    `json:"drive_id,omitempty"`
}

type User struct {
	UserID          string `json:"user_id"`
	NickName        string `json:"nick_name"`
	DefaultDriveID  string `json:"default_drive_id"`
	ResourceDriveID string `json:"resource_drive_id,omitempty"`
	BackupDriveID   string `json:"backup_drive_id,omitempty"`
}

type File struct {
	FileID       string `json:"file_id"`
	DriveID      string `json:"drive_id,omitempty"`
	ParentFileID string `json:"parent_file_id,omitempty"`
	Name         string `json:"name"`
	Type         string `json:"type"`
	Size         int64  `json:"size"`
	CreatedAt    string `json:"created_at,omitempty"`
	UpdatedAt    string `json:"updated_at,omitempty"`
	ContentHash  string `json:"content_hash,omitempty"`
}

type Page struct {
	Items      []File `json:"items"`
	NextMarker string `json:"next_marker"`
}

type Mutation struct {
	Operation   string `json:"operation"`
	DryRun      bool   `json:"dry_run"`
	File        File   `json:"file"`
	Destination string `json:"destination,omitempty"`
	Created     []File `json:"created,omitempty"`
	AsyncTaskID string `json:"async_task_id,omitempty"`
}

type Transfer struct {
	Operation string `json:"operation"`
	DryRun    bool   `json:"dry_run"`
	File      File   `json:"file"`
	LocalPath string `json:"local_path"`
	Bytes     int64  `json:"bytes"`
}

type LoginSession struct {
	Status    string `json:"status"`
	QRCodeURL string `json:"qr_code_url"`
	QRCodePNG string `json:"qr_code_png"`
}

type LoginResult struct {
	Status string `json:"status"`
	User   *User  `json:"user,omitempty"`
}

type AuthStatus struct {
	Authenticated bool      `json:"authenticated"`
	ConfigDir     string    `json:"config_dir"`
	ExpiresAt     time.Time `json:"expires_at,omitempty"`
	User          *User     `json:"user,omitempty"`
}
