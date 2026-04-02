package github

type Service interface {
	CreateIssue(title, body string, labels []string) (string, error)
	CreatePR(title, body, head, base string) (string, error)
	GetIssues(state string) ([]Issue, error)
}

type Issue struct {
	Number int    `json:"number"`
	Title  string `json:"title"`
	State  string `json:"state"`
	Body   string `json:"body"`
}
