// The forge's REST v3 surface under /api/v3: repositories, branches, commits, refs, contents,
// labels, issues, comments and pull requests, all over the one store in store.ts's model
// (store.go). Every reply is JSON with an X-GitHub-Request-Id; every error is {message}.

package main

import (
	"encoding/base64"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
)

type restCtx struct {
	forge    *Forge
	w        http.ResponseWriter
	r        *http.Request
	query    url.Values
	rawQuery string
	// The request's JSON body, parsed on first use.
	bodyParsed bool
	bodyValue  map[string]any
	bodyBad    bool
}

// The request's JSON body: {} for an empty body, not-ok (after answering 400) for malformed JSON.
func (ctx *restCtx) jsonBody(raw []byte) (map[string]any, bool) {
	if ctx.bodyParsed {
		if ctx.bodyBad {
			return nil, false
		}
		return ctx.bodyValue, true
	}
	ctx.bodyParsed = true
	if len(raw) == 0 {
		ctx.bodyValue = map[string]any{}
		return ctx.bodyValue, true
	}
	value, err := parseJSON(raw)
	if err != nil {
		ctx.bodyBad = true
		sendError(ctx.w, 400, "Problems parsing JSON")
		return nil, false
	}
	if record := asRecord(value); record != nil {
		ctx.bodyValue = record
	} else {
		ctx.bodyValue = map[string]any{}
	}
	return ctx.bodyValue, true
}

// A query parameter read with JavaScript's Number() semantics (absent reads as 0).
func queryFloat(query url.Values, key string) float64 {
	values, ok := query[key]
	if !ok || len(values) == 0 {
		return 0
	}
	parsed, err := strconv.ParseFloat(strings.TrimSpace(values[0]), 64)
	if err != nil {
		return math.NaN()
	}
	return parsed
}

type queryPair struct{ key, val string }

func parseQueryPairs(raw string) []queryPair {
	pairs := []queryPair{}
	if raw == "" {
		return pairs
	}
	for _, part := range strings.Split(raw, "&") {
		if part == "" {
			continue
		}
		key, val := part, ""
		if index := strings.IndexByte(part, '='); index >= 0 {
			key, val = part[:index], part[index+1:]
		}
		if decoded, err := url.QueryUnescape(key); err == nil {
			key = decoded
		}
		if decoded, err := url.QueryUnescape(val); err == nil {
			val = decoded
		}
		pairs = append(pairs, queryPair{key, val})
	}
	return pairs
}

// One page of a list plus the GitHub `Link` header value (same-origin absolute URLs).
func (ctx *restCtx) paginate(path string, items []any) ([]any, string) {
	perPageRaw := queryFloat(ctx.query, "per_page")
	perPage := 30
	if perPageRaw > 0 {
		perPage = int(math.Trunc(perPageRaw))
		if perPage > 100 {
			perPage = 100
		}
	}
	pageRaw := queryFloat(ctx.query, "page")
	page := 1
	if pageRaw > 0 {
		page = int(math.Trunc(pageRaw))
	}
	totalPages := int(math.Ceil(float64(len(items)) / float64(perPage)))
	if totalPages < 1 {
		totalPages = 1
	}
	start := (page - 1) * perPage
	var slice []any
	if start < len(items) {
		end := start + perPage
		if end > len(items) {
			end = len(items)
		}
		slice = items[start:end]
	} else {
		slice = []any{}
	}
	if len(items) <= perPage && page == 1 {
		return slice, ""
	}
	host := ctx.r.Host
	if host == "" {
		host = "localhost"
	}
	urlFor := func(target int) string {
		pairs := parseQueryPairs(ctx.rawQuery)
		set := func(key, value string) {
			for index, pair := range pairs {
				if pair.key == key {
					pairs[index].val = value
					// URLSearchParams.set drops later duplicates of the key.
					kept := pairs[:index+1]
					for _, rest := range pairs[index+1:] {
						if rest.key != key {
							kept = append(kept, rest)
						}
					}
					pairs = kept
					return
				}
			}
			pairs = append(pairs, queryPair{key, value})
		}
		set("per_page", strconv.Itoa(perPage))
		set("page", strconv.Itoa(target))
		encoded := make([]string, len(pairs))
		for index, pair := range pairs {
			encoded[index] = url.QueryEscape(pair.key) + "=" + url.QueryEscape(pair.val)
		}
		return "http://" + host + path + "?" + strings.Join(encoded, "&")
	}
	links := []string{}
	if page < totalPages {
		links = append(links, "<"+urlFor(page+1)+">; rel=\"next\"", "<"+urlFor(totalPages)+">; rel=\"last\"")
	}
	if page > 1 {
		links = append(links, "<"+urlFor(1)+">; rel=\"first\"", "<"+urlFor(page-1)+">; rel=\"prev\"")
	}
	return slice, strings.Join(links, ", ")
}

// Send a paginated JSON list.
func (ctx *restCtx) sendList(path string, items []any) {
	page, link := ctx.paginate(path, items)
	headers := map[string]string{}
	if link != "" {
		headers["link"] = link
	}
	sendJson(ctx.w, 200, page, headers)
}

// The repository a /repos/{owner}/{repo} route names, answering 404 when it is not the viewer's.
func (ctx *restCtx) repoOr404(owner, name string) *RepoRec {
	repo := ctx.forge.GetRepo(owner, name)
	if repo == nil {
		sendError(ctx.w, 404, "Not Found")
		return nil
	}
	return repo
}

// JavaScript's Number(text) restricted to integers: (value, isInteger).
func jsInt(text string) (int, bool) {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return 0, true
	}
	parsed, err := strconv.ParseFloat(trimmed, 64)
	if err != nil || math.IsNaN(parsed) || math.IsInf(parsed, 0) || parsed != math.Trunc(parsed) {
		return 0, false
	}
	return int(parsed), true
}

func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for _, ch := range s {
		if ch < '0' || ch > '9' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------------------------
// repositories, branches, commits, refs, contents

func handleRoot(ctx *restCtx) {
	sendJson(ctx.w, 200, JObj{
		"current_user_url": "/api/v3/user",
		"repository_url":   "/api/v3/repos/{owner}/{repo}",
		"issue_search_url": "/api/v3/search/issues?q={query}",
		"rate_limit_url":   "/api/v3/rate_limit",
	}, map[string]string{"x-oauth-scopes": "repo, read:org"})
}

func handleCreateRepo(ctx *restCtx, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	name := strField(body, "name")
	if name == nil || *name == "" || strings.Contains(*name, "/") || *name == "." || *name == ".." {
		sendError(ctx.w, 422, "Validation Failed: name is missing or invalid")
		return
	}
	if ctx.forge.GetRepo(ViewerLogin, *name) != nil {
		sendError(ctx.w, 422, "Validation Failed: name already exists on this account")
		return
	}
	isPrivate := false
	if value, ok := body["private"].(bool); ok {
		isPrivate = value
	}
	description := ""
	if value := strField(body, "description"); value != nil {
		description = *value
	}
	repo := ctx.forge.CreateRepo(ViewerLogin, *name, isPrivate, description)
	sendJson(ctx.w, 201, repoJson(repo), nil)
}

func handlePatchRepo(ctx *restCtx, repo *RepoRec, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	if description := strField(body, "description"); description != nil {
		repo.Description = *description
	}
	if value, ok := body["private"].(bool); ok {
		repo.IsPrivate = value
	}
	if defaultBranch := strField(body, "default_branch"); defaultBranch != nil && *defaultBranch != "" {
		repo.DefaultBranch = *defaultBranch
	}
	if name := strField(body, "name"); name != nil && *name != "" && *name != repo.Name && !strings.Contains(*name, "/") {
		delete(ctx.forge.Repos, repoKey(repo.Owner, repo.Name))
		repo.Name = *name
		ctx.forge.Repos[repoKey(repo.Owner, repo.Name)] = repo
	}
	repo.UpdatedTs = ctx.forge.NextTs()
	sendJson(ctx.w, 200, repoJson(repo), nil)
}

// Parse an identity line `Name <email> ts tz` into the REST author shape.
func identityJson(line string) JObj {
	fallback := JObj{"name": line, "email": "", "date": isoOf(0)}
	if len(line) < 6 {
		return fallback
	}
	zone := line[len(line)-5:]
	if (zone[0] != '+' && zone[0] != '-') || !allDigits(zone[1:]) || line[len(line)-6] != ' ' {
		return fallback
	}
	core := line[:len(line)-6]
	lastSpace := strings.LastIndexByte(core, ' ')
	if lastSpace < 0 {
		return fallback
	}
	tsText := core[lastSpace+1:]
	if !allDigits(tsText) {
		return fallback
	}
	core2 := core[:lastSpace]
	if !strings.HasSuffix(core2, ">") {
		return fallback
	}
	idx := strings.LastIndex(core2, " <")
	if idx < 0 {
		return fallback
	}
	ts, _ := strconv.ParseInt(tsText, 10, 64)
	return JObj{"name": core2[:idx], "email": core2[idx+2 : len(core2)-1], "date": isoOf(ts)}
}

func commitJson(repo *RepoRec, oid string) JObj {
	object := repo.Objects[oid]
	if object == nil || object.Type != ObjCommit {
		return JObj{"sha": oid}
	}
	parsed := parseCommit(object.Data)
	parents := []any{}
	for _, parent := range parsed.Parents {
		parents = append(parents, JObj{"sha": parent})
	}
	user := JObj{"login": ViewerLogin, "id": ViewerID, "type": "User"}
	return JObj{
		"sha": oid,
		"commit": JObj{
			"author":    identityJson(parsed.Author),
			"committer": identityJson(parsed.Committer),
			"message":   strings.TrimSuffix(parsed.Message, "\n"),
			"tree":      JObj{"sha": parsed.Tree},
		},
		"author":    user,
		"committer": user,
		"parents":   parents,
	}
}

func handleListCommits(ctx *restCtx, repo *RepoRec) {
	tip := ""
	if shaParam, present := ctx.query["sha"]; present && len(shaParam) > 0 && shaParam[0] != "" {
		tip = ctx.forge.BranchTip(repo, shaParam[0])
		if tip == "" {
			if _, ok := repo.Objects[shaParam[0]]; ok {
				tip = shaParam[0]
			}
		}
	}
	if tip == "" {
		tip = ctx.forge.BranchTip(repo, repo.DefaultBranch)
	}
	if tip == "" {
		sendJson(ctx.w, 200, []any{}, nil)
		return
	}
	commits := []any{}
	for _, oid := range logFrom(repo, tip) {
		commits = append(commits, commitJson(repo, oid))
	}
	ctx.sendList("/api/v3/repos/"+repo.Owner+"/"+repo.Name+"/commits", commits)
}

func handleCreateRef(ctx *restCtx, repo *RepoRec, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	ref := strField(body, "ref")
	sha := strField(body, "sha")
	if ref == nil || !strings.HasPrefix(*ref, "refs/") || sha == nil {
		sendError(ctx.w, 422, "Validation Failed: ref or sha is invalid")
		return
	}
	object, exists := repo.Objects[*sha]
	if !exists {
		sendError(ctx.w, 422, "Validation Failed: ref or sha is invalid")
		return
	}
	if _, exists := repo.Refs[*ref]; exists {
		sendError(ctx.w, 422, "Reference already exists")
		return
	}
	repo.Refs[*ref] = *sha
	sendJson(ctx.w, 201, JObj{"ref": *ref, "object": JObj{"sha": *sha, "type": string(object.Type)}}, nil)
}

func refJson(repo *RepoRec, ref string) JObj {
	sha := repo.Refs[ref]
	objType := "commit"
	if object := repo.Objects[sha]; object != nil {
		objType = string(object.Type)
	}
	return JObj{"ref": ref, "object": JObj{"sha": sha, "type": objType}}
}

// Resolve a contents `ref` (branch, tag, commit or tree id) to a tree id.
func resolveTree(repo *RepoRec, ref *string) string {
	if ref == nil || *ref == "" {
		tip, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]
		if !ok {
			return ""
		}
		return commitTree(repo, tip)
	}
	if branchTip, ok := repo.Refs["refs/heads/"+*ref]; ok {
		return commitTree(repo, branchTip)
	}
	if tagTip, ok := repo.Refs["refs/tags/"+*ref]; ok {
		if object := repo.Objects[tagTip]; object != nil && object.Type == ObjCommit {
			return commitTree(repo, tagTip)
		}
		return ""
	}
	if object := repo.Objects[*ref]; object != nil {
		if object.Type == ObjCommit {
			return commitTree(repo, *ref)
		}
		if object.Type == ObjTree {
			return *ref
		}
	}
	return ""
}

func handleGetContents(ctx *restCtx, repo *RepoRec, path string) {
	var ref *string
	if values, present := ctx.query["ref"]; present && len(values) > 0 {
		ref = &values[0]
	}
	tree := resolveTree(repo, ref)
	if tree == "" {
		sendError(ctx.w, 404, "Not Found")
		return
	}
	found := lookupPath(repo, tree, path)
	if found == nil {
		sendError(ctx.w, 404, "Not Found")
		return
	}
	baseName := path
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		baseName = path[index+1:]
	}
	if found.Kind == "file" {
		blob := repo.Objects[found.Oid]
		if blob == nil {
			sendError(ctx.w, 404, "Not Found")
			return
		}
		sendJson(ctx.w, 200, JObj{
			"type":     "file",
			"encoding": "base64",
			"content":  base64.StdEncoding.EncodeToString(blob.Data),
			"name":     baseName,
			"path":     path,
			"sha":      found.Oid,
			"size":     len(blob.Data),
		}, nil)
		return
	}
	entries := listDir(repo, tree, path)
	sorted := append([]TreeEntry{}, entries...)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].Name < sorted[j].Name })
	listing := []any{}
	for _, entry := range sorted {
		isDir := entry.Mode == "40000"
		size := 0
		if !isDir {
			if blob := repo.Objects[entry.Oid]; blob != nil {
				size = len(blob.Data)
			}
		}
		entryPath := entry.Name
		if path != "" {
			entryPath = path + "/" + entry.Name
		}
		entryType := "file"
		if isDir {
			entryType = "dir"
		}
		listing = append(listing, JObj{
			"type": entryType,
			"name": entry.Name,
			"path": entryPath,
			"sha":  entry.Oid,
			"size": size,
		})
	}
	sendJson(ctx.w, 200, listing, nil)
}

// The branch a contents write targets, and its current tip ("" when the branch is unborn).
func contentsTarget(repo *RepoRec, branch string) (string, string) {
	ref := "refs/heads/" + branch
	if tip, ok := repo.Refs[ref]; ok {
		return ref, tip
	}
	if branch != repo.DefaultBranch {
		if fallback, ok := repo.Refs["refs/heads/"+repo.DefaultBranch]; ok {
			return ref, fallback
		}
	}
	return ref, ""
}

func handlePutContents(ctx *restCtx, repo *RepoRec, path string, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	content := strField(body, "content")
	if content == nil {
		sendError(ctx.w, 422, "Validation Failed: content is missing")
		return
	}
	message := "Update " + path
	if value := strField(body, "message"); value != nil {
		message = *value
	}
	branch := repo.DefaultBranch
	if value := strField(body, "branch"); value != nil {
		branch = *value
	}
	ref, parent := contentsTarget(repo, branch)
	parentTree := ""
	if parent != "" {
		parentTree = commitTree(repo, parent)
	}
	var existing *PathHit
	if parentTree != "" {
		existing = lookupPath(repo, parentTree, path)
	}
	if existing != nil && existing.Kind == "dir" {
		sendError(ctx.w, 422, "Validation Failed: path is a directory")
		return
	}
	shaGiven := strField(body, "sha")
	if existing != nil {
		if shaGiven == nil {
			sendError(ctx.w, 422, "Validation Failed: sha is required to update a file")
			return
		}
		if strings.ToLower(*shaGiven) != existing.Oid {
			sendError(ctx.w, 409, path+" does not match "+*shaGiven)
			return
		}
	} else if shaGiven != nil {
		sendError(ctx.w, 422, "Validation Failed: sha must not be given for a new file")
		return
	}
	data, decoded := base64LenientDecode(*content)
	if !decoded {
		data = []byte{}
	}
	blobOid := objPut(repo, ObjBlob, data)
	files := map[string]FlatFile{}
	if parentTree != "" {
		files = flattenTree(repo, parentTree)
	}
	mode := "100644"
	if existing != nil {
		mode = existing.Mode
	}
	files[path] = FlatFile{Oid: blobOid, Mode: mode}
	tree := treeFromFlat(repo, files)
	ts := ctx.forge.NextTs()
	identity := identityLine(ts)
	parents := []string{}
	if parent != "" {
		parents = []string{parent}
	}
	commitOid := objPut(repo, ObjCommit, buildCommit(tree, parents, message, identity, identity))
	repo.Refs[ref] = commitOid
	repo.UpdatedTs = ts
	baseName := path
	if index := strings.LastIndexByte(path, '/'); index >= 0 {
		baseName = path[index+1:]
	}
	status := 201
	if existing != nil {
		status = 200
	}
	sendJson(ctx.w, status, JObj{
		"content": JObj{
			"name":     baseName,
			"path":     path,
			"sha":      blobOid,
			"size":     len(data),
			"type":     "file",
			"encoding": "base64",
		},
		"commit": JObj{"sha": commitOid, "message": message, "author": identityJson(identity), "committer": identityJson(identity)},
	}, nil)
}

func handleDeleteContents(ctx *restCtx, repo *RepoRec, path string, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	branch := repo.DefaultBranch
	if value := strField(body, "branch"); value != nil {
		branch = *value
	}
	ref, parent := contentsTarget(repo, branch)
	parentTree := ""
	if parent != "" {
		parentTree = commitTree(repo, parent)
	}
	var existing *PathHit
	if parentTree != "" {
		existing = lookupPath(repo, parentTree, path)
	}
	if existing == nil || existing.Kind != "file" || parent == "" || parentTree == "" {
		sendError(ctx.w, 404, "Not Found")
		return
	}
	shaGiven := strField(body, "sha")
	shaText := ""
	if shaGiven != nil {
		shaText = *shaGiven
	}
	if shaGiven == nil || strings.ToLower(shaText) != existing.Oid {
		sendError(ctx.w, 409, path+" does not match "+shaText)
		return
	}
	files := flattenTree(repo, parentTree)
	delete(files, path)
	tree := treeFromFlat(repo, files)
	ts := ctx.forge.NextTs()
	identity := identityLine(ts)
	message := "Delete " + path
	if value := strField(body, "message"); value != nil {
		message = *value
	}
	commitOid := objPut(repo, ObjCommit, buildCommit(tree, []string{parent}, message, identity, identity))
	repo.Refs[ref] = commitOid
	repo.UpdatedTs = ts
	sendJson(ctx.w, 200, JObj{"content": nil, "commit": JObj{"sha": commitOid}}, nil)
}

// ---------------------------------------------------------------------------------------------
// labels, issues, comments, pull requests

func handleCreateLabel(ctx *restCtx, repo *RepoRec, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	name := strField(body, "name")
	if name == nil || *name == "" {
		sendError(ctx.w, 422, "Validation Failed: name is missing")
		return
	}
	for _, label := range repo.Labels {
		if strings.ToLower(label.Name) == strings.ToLower(*name) {
			sendError(ctx.w, 422, "Validation Failed: label already exists")
			return
		}
	}
	color := "ededed"
	if value := strField(body, "color"); value != nil {
		color = strings.TrimPrefix(*value, "#")
	}
	description := ""
	if value := strField(body, "description"); value != nil {
		description = *value
	}
	label := &LabelRec{ID: ctx.forge.NewLabelID(), Name: *name, Color: color, Description: description}
	repo.Labels[*name] = label
	sendJson(ctx.w, 201, labelJson(label), nil)
}

func issueLabelsOf(body map[string]any) []string {
	labels := []string{}
	if list, ok := body["labels"].([]any); ok {
		for _, entry := range list {
			if text, ok := entry.(string); ok {
				labels = append(labels, text)
			} else if record := asRecord(entry); record != nil {
				if name, ok := record["name"].(string); ok {
					labels = append(labels, name)
				}
			}
		}
	}
	return labels
}

func handleCreateIssue(ctx *restCtx, repo *RepoRec, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	result := createIssue(ctx.forge, repo, strField(body, "title"), strField(body, "body"), issueLabelsOf(body))
	if !result.OK {
		sendError(ctx.w, result.Status, result.Message)
		return
	}
	sendJson(ctx.w, 201, issueJson(repo, result.Issue), nil)
}

// Issues in list order: by number (creation), direction per the query (GitHub defaults to desc).
func sortedIssues(repo *RepoRec, query url.Values) []*IssueRec {
	state := "open"
	if values, present := query["state"]; present && len(values) > 0 {
		state = values[0]
	}
	direction := "desc"
	if values, present := query["direction"]; present && len(values) > 0 {
		direction = values[0]
	}
	issues := []*IssueRec{}
	for _, issue := range repo.Issues {
		issues = append(issues, issue)
	}
	if state == "open" {
		filtered := issues[:0]
		for _, issue := range issues {
			if issue.State == "open" {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	} else if state == "closed" {
		filtered := issues[:0]
		for _, issue := range issues {
			if issue.State == "closed" {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	if values, present := query["labels"]; present && len(values) > 0 && values[0] != "" {
		wanted := []string{}
		for _, name := range strings.Split(values[0], ",") {
			trimmed := strings.TrimSpace(name)
			if trimmed != "" {
				wanted = append(wanted, trimmed)
			}
		}
		filtered := issues[:0]
		for _, issue := range issues {
			hasAll := true
			for _, name := range wanted {
				found := false
				for _, labelName := range issue.LabelNames {
					if labelName == name {
						found = true
						break
					}
				}
				if !found {
					hasAll = false
					break
				}
			}
			if hasAll {
				filtered = append(filtered, issue)
			}
		}
		issues = filtered
	}
	sort.Slice(issues, func(i, j int) bool {
		if direction == "asc" {
			return issues[i].Number < issues[j].Number
		}
		return issues[i].Number > issues[j].Number
	})
	return issues
}

func (ctx *restCtx) issueOr404(repo *RepoRec, number int) *IssueRec {
	issue := repo.Issues[number]
	if issue == nil {
		sendError(ctx.w, 404, "Not Found")
		return nil
	}
	return issue
}

func handlePatchIssue(ctx *restCtx, repo *RepoRec, issue *IssueRec, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	if title := strField(body, "title"); title != nil {
		issue.Title = *title
	}
	if text := strField(body, "body"); text != nil {
		issue.Body = *text
	}
	if state := strField(body, "state"); state != nil && (*state == "closed" || *state == "open") {
		issue.State = *state
	}
	if list, present := body["labels"].([]any); present {
		names := []string{}
		for _, entry := range list {
			var name *string
			if text, ok := entry.(string); ok {
				name = &text
			} else if record := asRecord(entry); record != nil {
				if value, ok := record["name"].(string); ok {
					name = &value
				}
			}
			if name == nil {
				continue
			}
			if _, exists := repo.Labels[*name]; !exists {
				sendError(ctx.w, 422, "Validation Failed: label '"+*name+"' does not exist")
				return
			}
			names = append(names, *name)
		}
		issue.LabelNames = names
	}
	issue.UpdatedTs = ctx.forge.NextTs()
	sendJson(ctx.w, 200, issueJson(repo, issue), nil)
}

func handleCreateComment(ctx *restCtx, repo *RepoRec, issue *IssueRec, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	text := strField(body, "body")
	if text == nil {
		sendError(ctx.w, 422, "Validation Failed: body is missing")
		return
	}
	comment := &CommentRec{ID: ctx.forge.NewCommentID(), Body: *text, CreatedTs: ctx.forge.NextTs()}
	issue.Comments = append(issue.Comments, comment)
	sendJson(ctx.w, 201, commentJson(comment), nil)
}

func handleCreatePull(ctx *restCtx, repo *RepoRec, raw []byte) {
	body, ok := ctx.jsonBody(raw)
	if !ok {
		return
	}
	result := createPull(ctx.forge, repo, strField(body, "title"), strField(body, "body"), strField(body, "head"), strField(body, "base"))
	if !result.OK {
		sendError(ctx.w, result.Status, result.Message)
		return
	}
	sendJson(ctx.w, 201, pullJson(ctx.forge, repo, result.Pull), nil)
}

func (ctx *restCtx) pullOr404(repo *RepoRec, number int) *PullRec {
	pull := repo.Pulls[number]
	if pull == nil {
		sendError(ctx.w, 404, "Not Found")
		return nil
	}
	return pull
}

func handlePullFiles(ctx *restCtx, repo *RepoRec, pull *PullRec) {
	headTip := ctx.forge.BranchTip(repo, pull.HeadRef)
	if headTip == "" {
		headTip = pull.HeadShaAtCreate
	}
	baseTip := ctx.forge.BranchTip(repo, pull.BaseRef)
	if baseTip == "" {
		baseTip = pull.BaseShaAtCreate
	}
	mergeBase := mergeBaseOf(repo, baseTip, headTip)
	if mergeBase == "" {
		mergeBase = baseTip
	}
	changes := []any{}
	for _, change := range diffFiles(repo, mergeBase, headTip) {
		changes = append(changes, JObj{
			"filename":  change.Filename,
			"status":    change.Status,
			"additions": change.Additions,
			"deletions": change.Deletions,
			"changes":   change.Changes,
		})
	}
	sendJson(ctx.w, 200, changes, nil)
}

func handleMergePull(ctx *restCtx, repo *RepoRec, pull *PullRec) {
	if pull.State != "open" {
		sendError(ctx.w, 405, "Pull Request is not mergeable")
		return
	}
	sha, problem := ctx.forge.MergePull(repo, pull)
	if problem == "conflict" {
		sendError(ctx.w, 405, "Pull Request has merge conflicts")
		return
	}
	if problem != "" {
		sendError(ctx.w, 405, "Pull Request is not mergeable")
		return
	}
	sendJson(ctx.w, 200, JObj{"sha": sha, "merged": true, "message": "Pull Request successfully merged"}, nil)
}

// ---------------------------------------------------------------------------------------------
// routing

func routeRest(ctx *restCtx, segments []string, raw []byte) {
	w := ctx.w
	method := ctx.r.Method

	if len(segments) == 0 {
		if method == "GET" {
			handleRoot(ctx)
			return
		}
		sendError(w, 404, "Not Found")
		return
	}
	first := segments[0]
	var second, third string
	if len(segments) > 1 {
		second = segments[1]
	}
	if len(segments) > 2 {
		third = segments[2]
	}
	if first == "meta" && len(segments) == 1 && method == "GET" {
		sendJson(w, 200, JObj{"installed_version": "1.0.0", "enterprise": false}, nil)
		return
	}
	if first == "user" && len(segments) == 1 && method == "GET" {
		sendJson(w, 200, userJson(), nil)
		return
	}
	if first == "user" && second == "repos" && len(segments) == 2 {
		if method == "POST" {
			handleCreateRepo(ctx, raw)
			return
		}
		if method == "GET" {
			repos := []any{}
			for _, repo := range ctx.forge.ListRepos() {
				repos = append(repos, repoJson(repo))
			}
			ctx.sendList("/api/v3/user/repos", repos)
			return
		}
		sendError(w, 404, "Not Found")
		return
	}
	if first != "repos" || len(segments) < 3 {
		sendError(w, 404, "Not Found")
		return
	}
	owner, name := second, third
	rest := segments[3:]

	if len(rest) == 0 {
		if method == "DELETE" {
			if !ctx.forge.DeleteRepo(owner, name) {
				sendError(w, 404, "Not Found")
				return
			}
			sendNoContent(w)
			return
		}
		repo := ctx.repoOr404(owner, name)
		if repo == nil {
			return
		}
		if method == "GET" {
			sendJson(w, 200, repoJson(repo), nil)
			return
		}
		if method == "PATCH" {
			handlePatchRepo(ctx, repo, raw)
			return
		}
		sendError(w, 404, "Not Found")
		return
	}

	repo := ctx.repoOr404(owner, name)
	if repo == nil {
		return
	}
	repoPath := "/api/v3/repos/" + owner + "/" + name
	head := rest[0]

	if head == "branches" {
		if len(rest) == 1 && method == "GET" {
			branches := []any{}
			names := []string{}
			for ref := range repo.Refs {
				if strings.HasPrefix(ref, "refs/heads/") {
					names = append(names, ref[len("refs/heads/"):])
				}
			}
			sort.Strings(names)
			for _, branchName := range names {
				branches = append(branches, JObj{
					"name":      branchName,
					"commit":    JObj{"sha": repo.Refs["refs/heads/"+branchName]},
					"protected": false,
				})
			}
			sendJson(w, 200, branches, nil)
			return
		}
		if len(rest) >= 2 && method == "GET" {
			branch := strings.Join(rest[1:], "/")
			tip := ctx.forge.BranchTip(repo, branch)
			if tip == "" {
				sendError(w, 404, "Not Found")
				return
			}
			sendJson(w, 200, JObj{"name": branch, "commit": commitJson(repo, tip), "protected": false}, nil)
			return
		}
		sendError(w, 404, "Not Found")
		return
	}

	if head == "commits" {
		if len(rest) == 1 && method == "GET" {
			handleListCommits(ctx, repo)
			return
		}
		if len(rest) == 2 && method == "GET" {
			sha := rest[1]
			object, exists := repo.Objects[sha]
			if !exists || object.Type != ObjCommit {
				sendError(w, 404, "Not Found")
				return
			}
			sendJson(w, 200, commitJson(repo, sha), nil)
			return
		}
		sendError(w, 404, "Not Found")
		return
	}

	if head == "git" && len(rest) >= 2 && (rest[1] == "refs" || rest[1] == "ref") {
		if len(rest) == 2 && rest[1] == "refs" {
			if method == "POST" {
				handleCreateRef(ctx, repo, raw)
				return
			}
			if method == "GET" {
				refs := []string{}
				for ref := range repo.Refs {
					refs = append(refs, ref)
				}
				sort.Strings(refs)
				out := []any{}
				for _, ref := range refs {
					out = append(out, refJson(repo, ref))
				}
				sendJson(w, 200, out, nil)
				return
			}
			sendError(w, 404, "Not Found")
			return
		}
		if len(rest) >= 3 && method == "GET" {
			tail := strings.Join(rest[2:], "/")
			full := tail
			if !strings.HasPrefix(tail, "refs/") {
				full = "refs/" + tail
			}
			if _, exists := repo.Refs[full]; !exists {
				sendError(w, 404, "Not Found")
				return
			}
			sendJson(w, 200, refJson(repo, full), nil)
			return
		}
		sendError(w, 404, "Not Found")
		return
	}

	if head == "contents" {
		path := strings.Join(rest[1:], "/")
		if method == "GET" {
			handleGetContents(ctx, repo, path)
			return
		}
		if method == "PUT" {
			handlePutContents(ctx, repo, path, raw)
			return
		}
		if method == "DELETE" {
			handleDeleteContents(ctx, repo, path, raw)
			return
		}
		sendError(w, 404, "Not Found")
		return
	}

	if head == "labels" {
		if len(rest) == 1 {
			if method == "POST" {
				handleCreateLabel(ctx, repo, raw)
				return
			}
			if method == "GET" {
				labels := []*LabelRec{}
				for _, label := range repo.Labels {
					labels = append(labels, label)
				}
				sort.Slice(labels, func(i, j int) bool { return labels[i].Name < labels[j].Name })
				out := []any{}
				for _, label := range labels {
					out = append(out, labelJson(label))
				}
				sendJson(w, 200, out, nil)
				return
			}
			sendError(w, 404, "Not Found")
			return
		}
		if len(rest) == 2 && method == "GET" {
			wanted := strings.ToLower(rest[1])
			for _, label := range repo.Labels {
				if strings.ToLower(label.Name) == wanted {
					sendJson(w, 200, labelJson(label), nil)
					return
				}
			}
			sendError(w, 404, "Not Found")
			return
		}
		sendError(w, 404, "Not Found")
		return
	}

	if head == "issues" {
		if len(rest) == 1 {
			if method == "POST" {
				handleCreateIssue(ctx, repo, raw)
				return
			}
			if method == "GET" {
				issues := []any{}
				for _, issue := range sortedIssues(repo, ctx.query) {
					issues = append(issues, issueJson(repo, issue))
				}
				ctx.sendList(repoPath+"/issues", issues)
				return
			}
			sendError(w, 404, "Not Found")
			return
		}
		number, valid := jsInt(rest[1])
		if !valid || number < 1 {
			sendError(w, 404, "Not Found")
			return
		}
		if len(rest) == 2 {
			issue := ctx.issueOr404(repo, number)
			if issue == nil {
				return
			}
			if method == "GET" {
				sendJson(w, 200, issueJson(repo, issue), nil)
				return
			}
			if method == "PATCH" {
				handlePatchIssue(ctx, repo, issue, raw)
				return
			}
			sendError(w, 404, "Not Found")
			return
		}
		if len(rest) == 3 && rest[2] == "comments" {
			issue := ctx.issueOr404(repo, number)
			if issue == nil {
				return
			}
			if method == "POST" {
				handleCreateComment(ctx, repo, issue, raw)
				return
			}
			if method == "GET" {
				comments := []any{}
				for _, comment := range issue.Comments {
					comments = append(comments, commentJson(comment))
				}
				ctx.sendList(repoPath+"/issues/"+strconv.Itoa(number)+"/comments", comments)
				return
			}
			sendError(w, 404, "Not Found")
			return
		}
		sendError(w, 404, "Not Found")
		return
	}

	if head == "pulls" {
		if len(rest) == 1 {
			if method == "POST" {
				handleCreatePull(ctx, repo, raw)
				return
			}
			if method == "GET" {
				state := "open"
				if values, present := ctx.query["state"]; present && len(values) > 0 {
					state = values[0]
				}
				pulls := []*PullRec{}
				for _, pull := range repo.Pulls {
					if state == "open" && pull.State != "open" {
						continue
					}
					if state == "closed" && pull.State != "closed" {
						continue
					}
					pulls = append(pulls, pull)
				}
				sort.Slice(pulls, func(i, j int) bool { return pulls[i].Number > pulls[j].Number })
				out := []any{}
				for _, pull := range pulls {
					out = append(out, pullJson(ctx.forge, repo, pull))
				}
				ctx.sendList(repoPath+"/pulls", out)
				return
			}
			sendError(w, 404, "Not Found")
			return
		}
		number, valid := jsInt(rest[1])
		if !valid || number < 1 {
			sendError(w, 404, "Not Found")
			return
		}
		if len(rest) == 2 && method == "GET" {
			pull := ctx.pullOr404(repo, number)
			if pull == nil {
				return
			}
			sendJson(w, 200, pullJson(ctx.forge, repo, pull), nil)
			return
		}
		if len(rest) == 3 && rest[2] == "files" && method == "GET" {
			pull := ctx.pullOr404(repo, number)
			if pull == nil {
				return
			}
			handlePullFiles(ctx, repo, pull)
			return
		}
		if len(rest) == 3 && rest[2] == "merge" && method == "PUT" {
			pull := ctx.pullOr404(repo, number)
			if pull == nil {
				return
			}
			handleMergePull(ctx, repo, pull)
			return
		}
		sendError(w, 404, "Not Found")
		return
	}

	sendError(w, 404, "Not Found")
}
