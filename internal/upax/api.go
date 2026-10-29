package upax

// Wire types: the relay API (platform design doc § 6). Field names are the
// contract; don't rename them.

// PairRequest is POST /rpair/{code}.
type PairRequest struct {
	PublicKey string `json:"public_key"`
	HostName  string `json:"host_name"`
	OS        string `json:"os"`
	Arch      string `json:"arch"`
	Version   string `json:"version"`
}

// PairReply is its 202 answer.
type PairReply struct {
	RelayID     string `json:"relay_id"`
	Status      string `json:"status"`
	PollSeconds int    `json:"poll_seconds"`
}

// Relay and terminal statuses.
const (
	StatusActive  = "active"
	StatusPending = "pending"
	StatusPaused  = "paused"
)

// Connections.
const (
	ConnPull = "pull" // the relay reads the terminal over its SDK protocol
	ConnPush = "push" // the terminal pushes to the relay's LAN listener
)

// SyncRequest is POST /rly/{id}/sync.
type SyncRequest struct {
	Version   string           `json:"version"`
	HostName  string           `json:"host_name"`
	UptimeS   int64            `json:"uptime_s"`
	Terminals []TerminalReport `json:"terminals"`
}

// TerminalReport is one terminal's latest snapshot.
type TerminalReport struct {
	Serial       string `json:"serial"`
	Vendor       string `json:"vendor"`
	LANAddr      string `json:"lan_addr"`
	Connection   string `json:"connection"`
	Reachable    bool   `json:"reachable"`
	Model        string `json:"model,omitempty"`
	Firmware     string `json:"firmware,omitempty"`
	Users        int    `json:"users"`
	Fingerprints int    `json:"fingerprints"`
	Records      int    `json:"records"`
	// ClockSkewS is terminal clock minus the time in its UPAX zone (or the
	// relay's zone while UPAX hasn't given one). Nil when unknown.
	ClockSkewS *int `json:"clock_skew_s"`
	// ClockSkewEstimated marks a push terminal's skew, estimated from punch
	// arrival times rather than read from the terminal.
	ClockSkewEstimated bool   `json:"clock_skew_estimated,omitempty"`
	LastError          string `json:"last_error"`
}

// SyncReply is its 200 answer.
type SyncReply struct {
	Status            string          `json:"status"`
	PollSeconds       int             `json:"poll_seconds"`
	ErrorDelaySeconds int             `json:"error_delay_seconds"`
	Terminals         []TerminalState `json:"terminals"`
	Unregistered      []string        `json:"unregistered"`
}

// TerminalState is UPAX's view of a terminal registered to this relay.
type TerminalState struct {
	Serial    string    `json:"serial"`
	DeviceID  string    `json:"device_id"`
	Status    string    `json:"status"`
	Timezone  string    `json:"timezone"`
	ClockSync bool      `json:"clock_sync"`
	Commands  []Command `json:"commands"`
}

// Command is one queued command, in UPAX's command language (the ADMS
// syntax: "DATA UPDATE USERINFO PIN=…", "INFO", …).
type Command struct {
	Seq  int64  `json:"seq"`
	Line string `json:"line"`
}

// PunchBatch is POST /rly/{id}/punches.
type PunchBatch struct {
	Serial  string  `json:"serial"`
	Punches []Punch `json:"punches"`
}

// Punch is one attendance record, codes exactly as the terminal stores them.
type Punch struct {
	PIN      string `json:"pin"`
	Local    string `json:"local"` // "2006-01-02 15:04:05", terminal wall clock
	State    int    `json:"state"`
	Verify   int    `json:"verify"`
	WorkCode string `json:"workcode"`
}

type punchReply struct {
	Accepted int `json:"accepted"`
}

// Result is a command's outcome. Return 0 = done, anything else = failed.
type Result struct {
	Serial string `json:"serial"`
	Seq    int64  `json:"seq"`
	Return int    `json:"return"`
	Cmd    string `json:"cmd"`
}

// Command return codes (ADMS values, so a result means the same whichever
// way the command travelled).
const (
	ReturnOK          = 0
	ReturnError       = -1
	ReturnUnsupported = -1002
)

type resultsRequest struct {
	Results []Result `json:"results"`
}

type keyRequest struct {
	PublicKey string `json:"public_key"`
}
