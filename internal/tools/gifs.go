package tools

import (
	"context"
	"encoding/json"
	"strings"

	"parallax/internal/gifs"
	"parallax/internal/llm"
	"parallax/internal/projects"
)

type GIFEnv struct {
	Service    *gifs.Service
	Projects   *projects.Store
	ProjectID  string
	OnMutation func()
	OnApplied  func(rel string)
}

func RegisterGIFs(reg *Registry, env GIFEnv) {
	reg.Register(llm.NewFunctionTool(
		"search_gifs",
		"Search the combined GIPHY and KLIPY catalogs for an animated reaction, meme, gesture, or visual beat. Results are ranked across configured providers and include an import_ref. Search before importing; never invent an import_ref.",
		json.RawMessage(`{
			"type":"object",
			"properties":{
				"query":{"type":"string","description":"Exact concise GIF search phrase, maximum 50 characters"},
				"limit":{"type":"integer","minimum":1,"maximum":24,"description":"Maximum results; default 12"}
			},
			"required":["query"]
		}`),
	), env.search)

	reg.Register(llm.NewFunctionTool(
		"import_gif",
		"Import one result from search_gifs into the project media bin. Pass its exact import_ref. The imported animation is optimized as a timeline-ready video when the provider offers one; call place_media afterward only if the user asked to insert it on the timeline.",
		json.RawMessage(`{
			"type":"object",
			"properties":{"import_ref":{"type":"string","description":"Opaque import_ref returned by search_gifs"}},
			"required":["import_ref"]
		}`),
	), env.importGIF)
}

func (e GIFEnv) search(ctx context.Context, raw json.RawMessage) Result {
	if e.Service == nil {
		return Result{OK: false, Error: "GIF search is not configured"}
	}
	var in struct {
		Query string `json:"query"`
		Limit int    `json:"limit"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.Query) == "" {
		return Result{OK: false, Error: "query is required"}
	}
	if in.Limit == 0 {
		in.Limit = 12
	}
	response, err := e.Service.Search(ctx, in.Query, in.Limit)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	return Result{OK: true, Output: response}
}

func (e GIFEnv) importGIF(ctx context.Context, raw json.RawMessage) Result {
	if e.Service == nil || e.Projects == nil || e.ProjectID == "" {
		return Result{OK: false, Error: "GIF import requires an active project"}
	}
	var in struct {
		ImportRef string `json:"import_ref"`
	}
	if err := json.Unmarshal(raw, &in); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if strings.TrimSpace(in.ImportRef) == "" {
		return Result{OK: false, Error: "import_ref is required"}
	}
	download, err := e.Service.Fetch(ctx, in.ImportRef)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	defer download.Body.Close()
	media, err := e.Projects.SaveUpload(e.ProjectID, download.Name, download.Body)
	if err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	if err := e.Projects.SetMediaOrigin(e.ProjectID, media.Path, "gif"); err != nil {
		return Result{OK: false, Error: err.Error()}
	}
	media.Origin = "gif"
	if e.OnMutation != nil {
		e.OnMutation()
	}
	if e.OnApplied != nil {
		e.OnApplied(media.Path)
	}
	return Result{OK: true, Output: map[string]any{"path": media.Path, "name": media.Name, "kind": media.Kind, "bytes": media.Bytes}}
}
