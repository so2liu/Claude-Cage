package guestos

// MacLoginRequest is the local one-instance login control protocol. No guest or
// host file paths, clipboard contents, or global shortcuts are accepted. Only
// the authenticated host may attach a stored password to fill-password.
type MacLoginRequest struct {
	Action      string `json:"action"`
	Session     string `json:"session,omitempty"`
	Setup       bool   `json:"initial_setup,omitempty"`
	Maintenance string `json:"maintenance,omitempty"`
	Geometry    string `json:"geometry,omitempty"`
	Code        int    `json:"code"`
	Shift       bool   `json:"shift"`
	X           int    `json:"x"`
	Y           int    `json:"y"`
	DeltaX      int    `json:"delta_x,omitempty"`
	DeltaY      int    `json:"delta_y,omitempty"`
	Password    string `json:"password,omitempty"`
}
type MacLoginReply struct {
	DesktopVersion     int    `json:"desktop_version,omitempty"`
	MIME               string `json:"mime,omitempty"`
	Session            string `json:"session,omitempty"`
	Image              []byte `json:"image,omitempty"`
	Error              string `json:"error,omitempty"`
	Closed             bool   `json:"closed,omitempty"`
	Geometry           string `json:"geometry,omitempty"`
	Region             []int  `json:"region,omitempty"`
	Recovery           string `json:"recovery,omitempty"`
	Administrator      string `json:"administrator,omitempty"`
	MaintenanceVersion int    `json:"maintenance_version,omitempty"`
}

func ValidMacMaintenancePurpose(value string) bool {
	return value == "projects" || value == "screen" || value == "input"
}
