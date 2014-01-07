package template

import (
	"bytes"
	"errors"
	"fmt"
	"gnd.la/html"
	"gnd.la/loaders"
	"gnd.la/log"
	"gnd.la/template/assets"
	"gnd.la/util"
	"gnd.la/util/internal/templateutil"
	"gnd.la/util/textutil"
	"html/template"
	"io"
	"io/ioutil"
	"mime"
	"net/http"
	"path"
	"regexp"
	"strconv"
	"strings"
	"text/template/parse"
)

type FuncMap map[string]interface{}
type VarMap map[string]interface{}

const (
	leftDelim             = "{{"
	rightDelim            = "}}"
	dataKey               = "Data"
	varsKey               = "Vars"
	topBoilerplateName    = "_gondola_top_hooks"
	bottomBoilerplateName = "_gondola_bottom_hooks"
	topAssetsFuncName     = "_gondola_topAssets"
	AssetFuncName         = "asset"
	bottomAssetsFuncName  = "_gondola_bottomAssets"
	topBoilerplate        = "{{ _gondola_topAssets }}"
	bottomBoilerplate     = "{{ _gondola_bottomAssets }}"
)

var (
	ErrNoAssetsManager       = errors.New("template does not have an assets manager")
	ErrAssetsAlreadyPrepared = errors.New("assets have been already prepared")
	commentRe                = regexp.MustCompile(`(?s:\{\{\\*(.*?)\*/\}\})`)
	keyRe                    = regexp.MustCompile(`(?s:\s*([\w\-_])+?(:|\|))`)
	defineRe                 = regexp.MustCompile(`(\{\{\s*?define.*?\}\})`)
	topTree                  = compileTree(topBoilerplate)
	bottomTree               = compileTree(bottomBoilerplate)
)

type Hook struct {
	Template *Template
	Position assets.Position
}

type Template struct {
	*template.Template
	Name          string
	Debug         bool
	Loader        loaders.Loader
	AssetsManager *assets.Manager
	Trees         map[string]*parse.Tree
	Minify        bool
	Final         bool
	funcMap       FuncMap
	vars          VarMap
	root          string
	assetGroups   []*assets.Group
	topAssets     template.HTML
	bottomAssets  template.HTML
	contentType   string
	hooks         []*Hook
}

func (t *Template) init() {
	t.Template = template.New("")
	// This is required so text/template calls t.init()
	// and initializes the common data structure
	t.Template.New("")
	funcs := FuncMap{
		topAssetsFuncName:    t.templateTopAssets,
		bottomAssetsFuncName: t.templateBottomAssets,
		AssetFuncName:        t.Asset,
	}
	t.Funcs(funcs)
	t.Template.Funcs(template.FuncMap(funcs)).Funcs(templateFuncs)
}

func (t *Template) Root() string {
	return t.root
}

func (t *Template) templateTopAssets() template.HTML {
	return t.topAssets
}

func (t *Template) templateBottomAssets() template.HTML {
	return t.bottomAssets
}

func (t *Template) Asset(arg string) (string, error) {
	if t.AssetsManager != nil {
		return t.AssetsManager.URL(arg), nil
	}
	return "", ErrNoAssetsManager
}

func (t *Template) Assets() []*assets.Group {
	return t.assetGroups
}

func (t *Template) AddAssets(groups []*assets.Group) error {
	if t.topAssets != "" || t.bottomAssets != "" {
		return ErrAssetsAlreadyPrepared
	}
	t.assetGroups = append(t.assetGroups, groups...)
	return nil
}

func (t *Template) PrepareAssets() error {
	if t.topAssets != "" || t.bottomAssets != "" {
		return ErrAssetsAlreadyPrepared
	}
	var groups [][]*assets.Group
	for _, v := range t.assetGroups {
		if (t.Debug && v.Options.NoDebug()) || (!t.Debug && v.Options.Debug()) {
			// Asset enabled only for debug or non-debug
			continue
		}
		if len(v.Assets) == 0 {
			continue
		}
		if v.Options.Bundle() && v.Options.Cdn() {
			return fmt.Errorf("asset group %s has incompatible options \"bundle\" and \"cdn\"", v.Names())
		}
		// Make a copy of the group, so assets get executed and compiled, every
		// time the template is loaded. This is specially useful while developing
		// a Gondola app which uses compilable or executable assets.
		v = copyGroup(v)
		// Check if any assets have to be compiled (LESS, CoffeScript, etc...)
		for _, a := range v.Assets {
			name, err := assets.Compile(v.Manager, a.Name, a.Type, v.Options)
			if err != nil {
				return fmt.Errorf("error compiling asset %q: %s", a.Name, err)
			}
			a.Name = name
		}
		added := false
		if v.Options.Bundable() {
			for ii, g := range groups {
				if g[0].Options.Bundable() || g[0].Options.Bundle() {
					if canBundle(g[0], v) {
						added = true
						groups[ii] = append(groups[ii], v)
						break
					}
				}
			}
		}
		if !added {
			groups = append(groups, []*assets.Group{v})
		}
	}
	var top bytes.Buffer
	var bottom bytes.Buffer
	for _, group := range groups {
		// Only bundle and use CDNs in non-debug mode
		if !t.Debug {
			if group[0].Options.Bundle() || group[0].Options.Bundable() {
				bundled, err := assets.Bundle(group, group[0].Options)
				if err == nil {
					group = []*assets.Group{
						&assets.Group{
							Manager: group[0].Manager,
							Assets:  []*assets.Asset{bundled},
							Options: group[0].Options,
						},
					}
				} else {
					var names []string
					for _, g := range group {
						for _, a := range g.Assets {
							names = append(names, a.Name)
						}
					}
					log.Errorf("error bundling assets %s: %s - using individual assets", names, err)
				}
			} else if group[0].Options.Cdn() {
				for _, g := range group {
					for _, a := range g.Assets {
						cdn, err := assets.Cdn(a.Name)
						if err != nil {
							if f, _, _ := g.Manager.Load(a.Name); f != nil {
								f.Close()
								log.Errorf("could not find CDN for asset %q: %s - using local copy", a.Name, err)
							} else {
								return fmt.Errorf("could not find CDN for asset %q: %s", a.Name, err)
							}
						} else {
							a.Name = cdn
						}
					}
				}
			}
		}
		for _, g := range group {
			for _, v := range g.Assets {
				switch v.Position {
				case assets.Top:
					if err := assets.RenderTo(&top, g.Manager, v); err != nil {
						return fmt.Errorf("error rendering asset %q", v.Name)
					}
					top.WriteByte('\n')
				case assets.Bottom:
					if err := assets.RenderTo(&bottom, g.Manager, v); err != nil {
						return fmt.Errorf("error rendering asset %q", v.Name)
					}
					bottom.WriteByte('\n')
				default:
					return fmt.Errorf("asset %q has invalid position %s", v.Name, v.Position)
				}
			}
		}
	}
	t.topAssets = template.HTML(top.String())
	t.bottomAssets = template.HTML(bottom.String())
	return nil
}

func (t *Template) Hook(hook *Hook) error {
	for _, h := range hook.Template.hooks {
		if err := t.Hook(h); err != nil {
			return err
		}
	}
	ha := hook.Template.Assets()
	if len(ha) > 0 {
		if err := t.AddAssets(ha); err != nil {
			return err
		}
	}
	for k, v := range hook.Template.Trees {
		if _, ok := t.Trees[k]; ok {
			if k != topBoilerplateName && k != bottomBoilerplateName {
				return fmt.Errorf("duplicate template %q", k)
			}
		} else {
			if err := t.AddParseTree(k, v.Copy()); err != nil {
				return err
			}
		}
	}
	var key string
	switch hook.Position {
	case assets.Top:
		key = topBoilerplateName
	case assets.Bottom:
		key = bottomBoilerplateName
	case assets.None:
		// must be manually referenced from
		// another template
	default:
		return fmt.Errorf("invalid hook position %d", hook.Position)
	}
	if key != "" {
		node := &parse.TemplateNode{
			NodeType: parse.NodeTemplate,
			Name:     hook.Template.Root(),
			Pipe: &parse.PipeNode{
				NodeType: parse.NodePipe,
				Cmds: []*parse.CommandNode{
					&parse.CommandNode{
						NodeType: parse.NodeCommand,
						Args:     []parse.Node{&parse.DotNode{}},
					},
				},
			},
		}
		tree := t.Trees[key].Copy()
		tree.Root.Nodes = append(tree.Root.Nodes, node)
		t.Trees[key] = tree
	}
	if err := t.Rebuild(); err != nil {
		return err
	}
	t.hooks = append(t.hooks, hook)
	return nil
}

func (t *Template) evalCommentVar(varname string) (string, error) {
	return eval(t.vars, varname)
}

func (t *Template) parseCommentVariables(values []string) ([]string, error) {
	parsed := make([]string, len(values))
	for ii, v := range values {
		s := strings.Index(v, "{{")
		for s >= 0 {
			end := strings.Index(v[s:], "}}")
			if end < 0 {
				return nil, fmt.Errorf("unterminated variable %q", v[s:])
			}
			// Adjust end to be relative to the start of the string
			end += s
			varname := strings.TrimSpace(v[s+2 : end])
			if len(varname) == 0 {
				return nil, fmt.Errorf("empty variable name")
			}
			if varname[0] != '$' {
				return nil, fmt.Errorf("invalid variable name %q, must start with $", varname)
			}
			varname = varname[1:]
			if len(varname) == 0 {
				return nil, fmt.Errorf("empty variable name")
			}
			value, err := t.evalCommentVar(varname)
			if err != nil {
				return nil, fmt.Errorf("error evaluating variable %q: %s", varname, err)
			}
			v = v[:s] + value + v[end+2:]
			s = strings.Index(v, "{{")
		}
		parsed[ii] = v
	}
	return parsed, nil
}

func (t *Template) parseComment(comment string, file string, included bool) error {
	// Escaped newlines
	comment = strings.Replace(comment, "\\\n", " ", -1)
	lines := strings.Split(comment, "\n")
	extended := false
	for _, v := range lines {
		m := keyRe.FindStringSubmatchIndex(v)
		if m != nil && m[0] == 0 && len(m) > 3 {
			start := m[1] - m[3]
			end := start + m[2]
			key := strings.ToLower(strings.TrimSpace(v[start:end]))
			var options assets.Options
			var value string
			if len(v) > end {
				rem := v[end+1:]
				if v[end] == '|' {
					// Has options
					colon := strings.IndexByte(rem, ':')
					opts := rem[:colon]
					var err error
					options, err = assets.ParseOptions(opts)
					if err != nil {
						return fmt.Errorf("error parsing options for asset key %q: %s", key, err)
					}
					value = rem[colon+1:]
				} else {
					// No options
					value = rem
				}
			}
			splitted, err := textutil.SplitFields(value, ",")
			if err != nil {
				return fmt.Errorf("error parsing value for asset key %q: %s", key, err)
			}
			values, err := t.parseCommentVariables(splitted)
			if err != nil {
				return fmt.Errorf("error parsing values for asset key %q: %s", key, err)
			}
			for ii, val := range values {
				// Check if the asset is a template
				if val != "" && val[0] == '(' && val[len(val)-1] == ')' {
					name := val[1 : len(val)-1]
					var err error
					values[ii], err = executeAsset(t, name)
					if err != nil {
						return fmt.Errorf("error executing asset template %s: %s", name, err)
					}
				}
			}

			inc := true
			switch key {
			case "extend", "extends":
				if extended || len(values) > 1 {
					return fmt.Errorf("templates can only extend one template")
				}
				if t.Final {
					return fmt.Errorf("template has been declared as final")
				}
				if strings.ToLower(values[0]) == "none" {
					t.Final = true
					break
				}
				extended = true
				inc = false
				fallthrough
			case "include", "includes":
				for _, n := range values {
					err := t.load(n, inc)
					if err != nil {
						return err
					}
				}
			default:
				if t.AssetsManager == nil {
					return ErrNoAssetsManager
				}
				group, err := assets.Parse(t.AssetsManager, key, values, options)
				if err != nil {
					return err
				}
				t.assetGroups = append(t.assetGroups, group)
			}
		}
	}
	if !extended && !included {
		t.root = file
	}
	return nil
}

func (t *Template) loadText(name string) (string, error) {
	f, _, err := t.Loader.Load(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	b, err := ioutil.ReadAll(f)
	if err != nil {
		return "", err
	}
	if conv := converters[strings.ToLower(path.Ext(name))]; conv != nil {
		b, err = conv(b)
		if err != nil {
			return "", err
		}
	}
	s := string(b)
	return s, nil
}

func (t *Template) load(name string, included bool) error {
	// TODO: Detect circular dependencies
	s, err := t.loadText(name)
	if err != nil {
		return err
	}
	matches := commentRe.FindStringSubmatch(s)
	comment := ""
	if matches != nil && len(matches) > 0 {
		comment = matches[1]
	}
	err = t.parseComment(comment, name, included)
	if err != nil {
		return err
	}
	if idx := strings.Index(s, "</head>"); idx >= 0 {
		s = s[:idx] + fmt.Sprintf("{{ template %q . }}", topBoilerplateName) + s[idx:]
	}
	if idx := strings.Index(s, "</body>"); idx >= 0 {
		s = s[:idx] + fmt.Sprintf("{{ template %q . }}", bottomBoilerplateName) + s[idx:]
	}
	// The $Vars definition must be present at parse
	// time, because otherwise the parser will throw an
	// error when it finds a variable which wasn't
	// previously defined
	// Prepend to the template and to any define nodes found
	prepend := "{{ $Vars := .Vars }}"
	s = prepend + defineRe.ReplaceAllString(s, "$0"+strings.Replace(prepend, "$", "$$", -1))
	treeMap, err := parse.Parse(name, s, leftDelim, rightDelim, templateFuncs, t.funcMap)
	if err != nil {
		return err
	}
	var renames map[string]string
	for k, v := range treeMap {
		if _, contains := t.Trees[k]; contains {
			log.Debugf("Template %s redefined", k)
			// Redefinition of a template, which is allowed
			// by gondola templates. Just rename this
			// template and change any template
			// nodes referring to it in the final sweep
			if renames == nil {
				renames = make(map[string]string)
			}
			fk := k
			for {
				k += "_"
				if len(renames[fk]) < len(k) {
					renames[fk] = k
					break
				}
			}
		}
		t.rewriteTemplateNodes(v)
		err := t.AddParseTree(k, v)
		if err != nil {
			return err
		}
	}
	mimeType := mime.TypeByExtension(path.Ext(t.Name))
	if mimeType == "" {
		mimeType = "text/html; charset=utf-8"
	}
	t.contentType = mimeType
	if renames != nil {
		t.renameTemplates(renames)
	}
	return nil
}

func (t *Template) walkTrees(nt parse.NodeType, f func(parse.Node)) {
	for _, v := range t.Trees {
		templateutil.WalkTree(v, func(n, p parse.Node) {
			if n.Type() == nt {
				f(n)
			}
		})
	}
}

func (t *Template) referencedTemplates() []string {
	var templates []string
	t.walkTrees(parse.NodeTemplate, func(n parse.Node) {
		templates = append(templates, n.(*parse.TemplateNode).Name)
	})
	return templates
}

func (t *Template) assetFunc(arg string) (string, error) {
	if t.AssetsManager != nil {
		return t.AssetsManager.URL(arg), nil
	}
	return "", ErrNoAssetsManager
}

func (t *Template) prepareVars() error {
	for k, tr := range t.Trees {
		if len(tr.Root.Nodes) == 0 {
			// Empty template
			continue
		}
		if k == topBoilerplateName || k == bottomBoilerplateName {
			continue
		}
		// Skip the first node, since it sets $Vars.
		// Then wrap the rest of template in a WithNode, which sets
		// the dot to .Data
		field := &parse.FieldNode{
			NodeType: parse.NodeField,
			Ident:    []string{dataKey},
		}
		command := &parse.CommandNode{
			NodeType: parse.NodeCommand,
			Args:     []parse.Node{field},
		}
		pipe := &parse.PipeNode{
			NodeType: parse.NodePipe,
			Cmds:     []*parse.CommandNode{command},
		}
		var nodes []parse.Node
		nodes = append(nodes, tr.Root.Nodes[0])
		root := tr.Root.Nodes[1:]
		newRoot := &parse.ListNode{
			NodeType: parse.NodeList,
			Nodes:    root,
		}
		// The list needs to be copied, otherwise the
		// html/template escaper complains that the
		// node is shared between templates
		with := &parse.WithNode{
			parse.BranchNode{
				NodeType: parse.NodeWith,
				Pipe:     pipe,
				List:     newRoot,
				ElseList: newRoot.CopyList(),
			},
		}
		nodes = append(nodes, with)
		tr.Root = &parse.ListNode{
			NodeType: parse.NodeList,
			Nodes:    nodes,
		}
	}
	return nil
}

func (t *Template) renameTemplates(renames map[string]string) {
	t.walkTrees(parse.NodeTemplate, func(n parse.Node) {
		node := n.(*parse.TemplateNode)
		if rename, ok := renames[node.Name]; ok {
			node.Name = rename
		}
	})
}

func (t *Template) rewriteTemplateNodes(tree *parse.Tree) {
	// Rewrite any template nodes to pass also the variables, since
	// they are not inherited
	templateArgs := []parse.Node{
		parse.NewIdentifier("map"),
		&parse.StringNode{
			NodeType: parse.NodeString,
			Quoted:   fmt.Sprintf("%q", varsKey),
			Text:     varsKey,
		},
		&parse.VariableNode{
			NodeType: parse.NodeVariable,
			Ident:    []string{fmt.Sprintf("$%s", varsKey)},
		},
		&parse.StringNode{
			NodeType: parse.NodeString,
			Quoted:   fmt.Sprintf("%q", dataKey),
			Text:     dataKey,
		},
	}
	templateutil.WalkTree(tree, func(n, p parse.Node) {
		if n.Type() != parse.NodeTemplate {
			return
		}
		node := n.(*parse.TemplateNode)
		if node.Pipe == nil {
			// No data, just pass variables
			command := &parse.CommandNode{
				NodeType: parse.NodeCommand,
				Args:     templateArgs[:len(templateArgs)-1],
			}
			node.Pipe = &parse.PipeNode{
				NodeType: parse.NodePipe,
				Cmds:     []*parse.CommandNode{command},
			}
		} else {
			newPipe := &parse.PipeNode{
				NodeType: parse.NodePipe,
				Cmds:     node.Pipe.Cmds,
			}
			args := make([]parse.Node, len(templateArgs), len(templateArgs)+1)
			copy(args, templateArgs)
			command := &parse.CommandNode{
				NodeType: parse.NodeCommand,
				Args:     append(args, newPipe),
			}
			node.Pipe.Cmds = []*parse.CommandNode{command}
		}
	})
}

func (t *Template) Funcs(funcs FuncMap) {
	if t.funcMap == nil {
		t.funcMap = make(FuncMap)
	}
	for k, v := range funcs {
		t.funcMap[k] = v
	}
	t.Template.Funcs(template.FuncMap(t.funcMap))
}

func (t *Template) Include(name string) error {
	err := t.load(name, true)
	if err != nil {
		return err
	}
	return nil
}

// Parse parses the template starting with the given
// template name (and following any extends/includes
// directives declared in it).
func (t *Template) Parse(name string) error {
	return t.ParseVars(name, nil)
}

func (t *Template) ParseVars(name string, vars VarMap) error {
	t.Name = name
	t.vars = vars
	err := t.load(name, false)
	if err != nil {
		return err
	}
	// Add assets templates
	err = t.AddParseTree(topBoilerplateName, topTree)
	if err != nil {
		return err
	}
	err = t.AddParseTree(bottomBoilerplateName, bottomTree)
	if err != nil {
		return err
	}
	/*for _, v := range t.referencedTemplates() {
		if _, ok := t.Trees[v]; !ok {
			log.Debugf("adding missing template %q as empty", v)
			tree := compileTree("")
			t.AddParseTree(v, tree)
		}
	}*/
	if err := t.prepareVars(); err != nil {
		return err
	}
	return nil
}

func (t *Template) AddParseTree(name string, tree *parse.Tree) error {
	_, err := t.Template.AddParseTree(name, tree)
	if err != nil {
		panic(err)
		return err
	}
	t.Trees[name] = tree
	return nil
}

func (t *Template) Execute(w io.Writer, data interface{}) error {
	return t.ExecuteTemplateVars(w, "", data, nil)
}

func (t *Template) ExecuteTemplate(w io.Writer, name string, data interface{}) error {
	return t.ExecuteTemplateVars(w, name, data, nil)
}

func (t *Template) ExecuteVars(w io.Writer, data interface{}, vars VarMap) error {
	return t.ExecuteTemplateVars(w, "", data, vars)
}

func (t *Template) ExecuteTemplateVars(w io.Writer, name string, data interface{}, vars VarMap) error {
	templateData := map[string]interface{}{
		varsKey: vars,
		dataKey: data,
	}
	var buf bytes.Buffer
	if name == "" {
		name = t.root
	}
	err := t.Template.ExecuteTemplate(&buf, name, templateData)
	if err != nil {
		return err
	}
	if t.Minify {
		// Instead of using a new Buffer, make a copy of the []byte and Reset
		// buf. This minimizes the number of allocations while momentarily
		// using a bit more of memory than we need (exactly one byte per space
		// removed in the output).
		b := buf.Bytes()
		bc := make([]byte, len(b))
		copy(bc, b)
		r := bytes.NewReader(bc)
		buf.Reset()
		if err := html.Minify(&buf, r); err != nil {
			return err
		}
	}
	if rw, ok := w.(http.ResponseWriter); ok {
		header := rw.Header()
		header.Set("Content-Type", t.contentType)
		header.Set("Content-Length", strconv.Itoa(buf.Len()))
	}
	_, err = w.Write(buf.Bytes())
	return err
}

// MustExecute works like Execute, but panics if there's an error
func (t *Template) MustExecute(w io.Writer, data interface{}) {
	err := t.Execute(w, data)
	if err != nil {
		log.Fatalf("Error executing template: %s\n", err)
	}
}

// Rebuild rebuilds the template from its trees. Calling this function
// is required in order to commit any modification to the template trees.
func (t *Template) Rebuild() error {
	// Since text/template won't let us remove nor replace a parse
	// tree, we have to create a new html/template from scratch
	// and add the trees we have.
	t.init()
	for k, v := range t.Trees {
		if err := t.AddParseTree(k, v); err != nil {
			return err
		}
	}
	return nil
}

// AddFuncs registers new functions which will be available to
// the templates. Please, note that you must register the functions
// before compiling a template that uses them, otherwise the template
// parser will return an error.
func AddFuncs(f FuncMap) {
	for k, v := range f {
		templateFuncs[k] = v
	}
}

// Returns a loader which loads templates from
// the tmpl directory, relative to the application
// binary.
func DefaultTemplateLoader() loaders.Loader {
	return loaders.FSLoader(util.RelativePath("tmpl"))
}

// New returns a new template with the given loader and assets
// manager. Please, refer to the documention in gnd.la/loaders
// and gnd.la/asssets for further information in those types.
// If the loader is nil, DefaultTemplateLoader() will be used.
func New(loader loaders.Loader, manager *assets.Manager) *Template {
	if loader == nil {
		loader = DefaultTemplateLoader()
	}
	t := &Template{
		Loader:        loader,
		AssetsManager: manager,
		Trees:         make(map[string]*parse.Tree),
	}
	t.init()
	return t
}

// Parse creates a new template using the given loader and manager and then
// parses the template with the given name.
func Parse(loader loaders.Loader, manager *assets.Manager, name string) (*Template, error) {
	t := New(loader, manager)
	err := t.Parse(name)
	if err != nil {
		return nil, err
	}
	return t, nil
}

// MustParse works like parse, but panics if there's an error
func MustParse(loader loaders.Loader, manager *assets.Manager, name string) *Template {
	t, err := Parse(loader, manager, name)
	if err != nil {
		log.Fatalf("Error loading template %s: %s\n", name, err)
	}
	return t
}

func compileTree(text string) *parse.Tree {
	funcs := template.FuncMap{
		topAssetsFuncName:    func() {},
		bottomAssetsFuncName: func() {},
	}
	treeMap, err := parse.Parse("", text, leftDelim, rightDelim, funcs)
	if err != nil {
		panic(err)
	}
	return treeMap[""]
}

func canBundle(g1, g2 *assets.Group) bool {
	if g1.Manager == g2.Manager {
		if len(g1.Assets) > 0 && len(g2.Assets) > 0 {
			f1 := g1.Assets[0]
			f2 := g2.Assets[0]
			return f1.Type == f2.Type && f1.Position == f2.Position
		}
	}
	return false
}

func copyGroup(src *assets.Group) *assets.Group {
	copies := make([]*assets.Asset, len(src.Assets))
	for ii, v := range src.Assets {
		a := *v
		copies[ii] = &a
	}
	return &assets.Group{
		Manager: src.Manager,
		Assets:  copies,
		Options: src.Options,
	}
}
