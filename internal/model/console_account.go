package model

// ConsoleAccountService supplies the single operator's browser credentials.
// Tokens remain available for CLI use and administrator recovery.
type ConsoleAccountService interface {
	ConsoleAccount(Principal) (ConsoleAccount, error)
	AuthenticateConsole(username, password string) (Principal, error)
	SaveConsoleAccount(Principal, string, string, string) (ConsoleAccount, error)
}

type ConsoleAccount struct {
	Configured bool   `json:"configured"`
	Username   string `json:"username,omitempty"`
}
