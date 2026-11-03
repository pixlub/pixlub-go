package pixlub

// Tool is a tool of the catalog returned by Tools.List.
type Tool struct {
	// ID is the identifier to pass to Tools.Run, such as "removeBgApi".
	ID          string
	Name        string
	Description string
	Category    string
	// Executable is false when the tool cannot be run through the API.
	Executable bool
	// Handler is "local", "remote" or "none".
	Handler string
	// Inputs lists the file inputs. Pass them by name with RunRequest.Inputs,
	// or RunRequest.InputLists for several files.
	Inputs []ToolInput
	// Outputs lists the files a run may produce, by key (see Output.Key).
	Outputs []ToolOutput
}

// ToolInput is one file input of a tool.
type ToolInput struct {
	// ID is the name to use as a key of RunRequest.Inputs or RunRequest.InputLists.
	ID          string
	Label       string
	Description string
	// Required is true when the tool cannot run without this input.
	Required bool
	// Type is "image", "channel" (read as grayscale), "batch" or another kind
	// of file the tool reads. A palette is not a file: its colors go in Params.
	Type string
	// Multiple is true when the input takes several files: pass them with
	// RunRequest.InputLists.
	Multiple bool
	// PerFile is true when each file of the input is a run of its own, charged
	// on its own: a batch. Its outputs are keyed by rank, such as "out#0".
	PerFile bool
	// MaxFiles is the most files the input takes in one run, for your plan, or 0.
	MaxFiles int
	// Accept lists the media types the input takes, such as "image/png".
	Accept []string
}

// ToolOutput is one kind of file a tool produces.
type ToolOutput struct {
	// Key identifies the output in Result.Outputs (Output.Key). In a batch,
	// each file is keyed with its rank: "out#0", "out#1"...
	Key         string
	Label       string
	Description string
	// Kind is the kind of file, such as "image" or "data".
	Kind string
	// Multiple is true when a run may produce several files for this key.
	Multiple bool
	// ContentTypes lists the media types the output may have.
	ContentTypes []string
}
