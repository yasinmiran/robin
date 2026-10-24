package protocol

// BulkFile is the bulk kind of a whole file. From the browser it is an
// upload into the host's download directory; from the agent it is a file a
// program on the host offered and the user then asked for. ID ties the
// transfer to its upload or offer, and Name is the file's base name.
const BulkFile = "file"

// MaxFile bounds one file in either direction. The page holds a download
// in memory until the last byte is in, so this bounds the browser's
// memory as well as the transfer.
const MaxFile = 4 << 30

// Control types for files.
const (
	// ControlFileOffer goes from agent to browser when a program on the host
	// offers a file (robin-agent send), and again for every offer still
	// open when a page connects. The page shows it and fetches nothing until
	// the user asks.
	ControlFileOffer ControlType = "file-offer"
	// ControlFileWithdraw goes from agent to browser when an offer is gone:
	// the program that made it quit or was interrupted.
	ControlFileWithdraw ControlType = "file-withdraw"
	// ControlFileFetch goes from browser to agent when the user asks to
	// save an offered file. The agent answers with a BulkFile transfer that
	// carries the offer's ID.
	ControlFileFetch ControlType = "file-fetch"
	// ControlFileDecline goes from browser to agent when the user dismisses
	// an offer.
	ControlFileDecline ControlType = "file-decline"
	// ControlFileSaved goes from agent to browser once an upload is kept,
	// with the name it was kept under.
	ControlFileSaved ControlType = "file-saved"
)

// File is the file a file control message is about. Fetch, decline and
// withdraw carry only its ID.
type File struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Size int64  `json:"size,omitempty"`
}
