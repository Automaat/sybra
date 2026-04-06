package todoist

// Task represents a task from the Todoist REST API v2.
type Task struct {
	ID          string   `json:"id"`
	Content     string   `json:"content"`
	Description string   `json:"description"`
	ProjectID   string   `json:"project_id"`
	SectionID   string   `json:"section_id"`
	Priority    int      `json:"priority"` // 1=normal … 4=urgent (inverted from UI)
	Labels      []string `json:"labels"`
	Due         *DueDate `json:"due"`
	IsCompleted bool     `json:"is_completed"`
	CreatedAt   string   `json:"created_at"`
	URL         string   `json:"url"`
}

// DueDate holds the due-date info for a Todoist task.
type DueDate struct {
	Date        string `json:"date"`
	IsRecurring bool   `json:"is_recurring"`
	String      string `json:"string"`
}

// Project represents a Todoist project (subset of API fields).
type Project struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}
