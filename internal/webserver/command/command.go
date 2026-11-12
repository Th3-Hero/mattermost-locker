// Package command implements the /lock-bot slash command.
package command

// Request is the subset of Mattermost's slash command payload that we use.
type Request struct {
	Command     string
	Text        string
	UserName    string
	UserID      string
	ChannelID   string
	TeamID      string
	ResponseURL string
	Token       string
}

// Response is the JSON body returned to Mattermost.
type Response struct {
	ResponseType string `json:"response_type"`
	Text         string `json:"text"`
}

// Ephemeral builds a response only the invoking user can see.
func Ephemeral(text string) *Response {
	return &Response{ResponseType: "ephemeral", Text: text}
}
