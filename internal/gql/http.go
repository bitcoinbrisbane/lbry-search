package gql

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/graphql-go/graphql"
	"github.com/graphql-go/graphql/gqlerrors"
	"github.com/graphql-go/graphql/language/ast"
	"github.com/graphql-go/graphql/language/parser"
	"github.com/graphql-go/graphql/language/source"
)

// Limits keep one query from doing too much work.
const (
	MaxQueryBytes = 16 << 10
	MaxDepth      = 10
	// MaxCost is the most fields a query may ask for, counting a field inside a
	// list once per item it could return (a connection's `first`).
	MaxCost = 20000
)

type request struct {
	Query         string         `json:"query"`
	OperationName string         `json:"operationName"`
	Variables     map[string]any `json:"variables"`
}

// Handler serves POST (JSON body) and GET (?query=…) GraphQL requests, and
// GraphiQL to browsers when graphiql is true.
func (a *API) Handler(graphiql bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var req request
		switch r.Method {
		case http.MethodGet:
			if graphiql && r.URL.Query().Get("query") == "" && strings.Contains(r.Header.Get("Accept"), "text/html") {
				w.Header().Set("Content-Type", "text/html; charset=utf-8")
				io.WriteString(w, graphiqlPage)
				return
			}
			v := r.URL.Query()
			req.Query = v.Get("query")
			req.OperationName = v.Get("operationName")
			if s := v.Get("variables"); s != "" {
				if err := json.Unmarshal([]byte(s), &req.Variables); err != nil {
					writeErrors(w, http.StatusBadRequest, "variables: "+err.Error())
					return
				}
			}
		case http.MethodPost:
			body, err := io.ReadAll(io.LimitReader(r.Body, MaxQueryBytes+4096))
			if err != nil {
				writeErrors(w, http.StatusBadRequest, err.Error())
				return
			}
			if strings.HasPrefix(r.Header.Get("Content-Type"), "application/graphql") {
				req.Query = string(body)
			} else if err := json.Unmarshal(body, &req); err != nil {
				writeErrors(w, http.StatusBadRequest, "body: "+err.Error())
				return
			}
		case http.MethodOptions:
			w.Header().Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
			w.Header().Set("Access-Control-Allow-Headers", "Content-Type")
			w.WriteHeader(http.StatusNoContent)
			return
		default:
			writeErrors(w, http.StatusMethodNotAllowed, "use GET or POST")
			return
		}
		res := a.Do(r, req)
		w.Header().Set("Content-Type", "application/json")
		if r.Method == http.MethodGet && len(res.Errors) == 0 {
			w.Header().Set("Cache-Control", "public, max-age=60")
		}
		json.NewEncoder(w).Encode(res)
	}
}

// Do checks a request against the limits and runs it.
func (a *API) Do(r *http.Request, req request) *graphql.Result {
	if req.Query == "" {
		return errResult("no query")
	}
	if len(req.Query) > MaxQueryBytes {
		return errResult(fmt.Sprintf("query is over %d bytes", MaxQueryBytes))
	}
	doc, err := parser.Parse(parser.ParseParams{Source: source.NewSource(&source.Source{Body: []byte(req.Query), Name: "GraphQL request"})})
	if err != nil {
		return &graphql.Result{Errors: gqlerrors.FormatErrors(err)}
	}
	if err := checkLimits(doc, req.Variables); err != nil {
		return errResult(err.Error())
	}
	return graphql.Do(graphql.Params{
		Schema:         a.Schema,
		RequestString:  req.Query,
		VariableValues: req.Variables,
		OperationName:  req.OperationName,
		Context:        a.withLoader(r.Context()),
	})
}

func errResult(msg string) *graphql.Result {
	return &graphql.Result{Errors: []gqlerrors.FormattedError{{Message: msg}}}
}

func writeErrors(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(errResult(msg))
}

// --- depth and cost ---------------------------------------------------------

// listFields multiply the cost of what's under them by their `first` argument.
var listFields = map[string]int{"search": 20, "autocomplete": 10, "claims": 20}

func checkLimits(doc *ast.Document, vars map[string]any) error {
	fragments := map[string]*ast.FragmentDefinition{}
	for _, d := range doc.Definitions {
		if f, ok := d.(*ast.FragmentDefinition); ok && f.Name != nil {
			fragments[f.Name.Value] = f
		}
	}
	w := walker{fragments: fragments, vars: vars}
	for _, d := range doc.Definitions {
		if op, ok := d.(*ast.OperationDefinition); ok {
			if err := w.walk(op.SelectionSet, 1, 1, map[string]bool{}); err != nil {
				return err
			}
		}
	}
	return nil
}

type walker struct {
	fragments map[string]*ast.FragmentDefinition
	vars      map[string]any
	cost      int
}

func (w *walker) walk(set *ast.SelectionSet, depth, mult int, inFragment map[string]bool) error {
	if set == nil {
		return nil
	}
	if depth > MaxDepth {
		return fmt.Errorf("query is nested more than %d levels deep", MaxDepth)
	}
	for _, sel := range set.Selections {
		switch s := sel.(type) {
		case *ast.Field:
			name := s.Name.Value
			if strings.HasPrefix(name, "__") {
				continue // introspection: fixed size, and GraphiQL's own query is deep
			}
			w.cost += mult
			if w.cost > MaxCost {
				return fmt.Errorf("query asks for too much (over %d fields counting list items); use smaller `first` values", MaxCost)
			}
			childMult := mult
			if def, ok := listFields[name]; ok {
				n := w.firstArg(s, def)
				if n > MaxPage {
					n = MaxPage
				}
				if n < 1 {
					n = 1
				}
				childMult *= n
			}
			if err := w.walk(s.SelectionSet, depth+1, childMult, inFragment); err != nil {
				return err
			}
		case *ast.InlineFragment:
			if err := w.walk(s.SelectionSet, depth, mult, inFragment); err != nil {
				return err
			}
		case *ast.FragmentSpread:
			name := s.Name.Value
			f := w.fragments[name]
			if f == nil || inFragment[name] {
				continue // unknown or cyclic: validation reports it
			}
			inFragment[name] = true
			err := w.walk(f.SelectionSet, depth, mult, inFragment)
			delete(inFragment, name)
			if err != nil {
				return err
			}
		}
	}
	return nil
}

func (w *walker) firstArg(f *ast.Field, def int) int {
	for _, arg := range f.Arguments {
		if arg.Name == nil || arg.Name.Value != "first" {
			continue
		}
		switch v := arg.Value.(type) {
		case *ast.IntValue:
			var n int
			fmt.Sscan(v.Value, &n)
			return n
		case *ast.Variable:
			switch n := w.vars[v.Name.Value].(type) {
			case float64:
				return int(n)
			case int:
				return n
			}
		}
	}
	return def
}

const graphiqlPage = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>lbry-search GraphQL</title>
<link rel="stylesheet" href="https://unpkg.com/graphiql@3.7.1/graphiql.min.css">
<style>html,body,#graphiql{height:100%;margin:0}</style>
</head>
<body>
<div id="graphiql">Loading…</div>
<script src="https://unpkg.com/react@18.3.1/umd/react.production.min.js"></script>
<script src="https://unpkg.com/react-dom@18.3.1/umd/react-dom.production.min.js"></script>
<script src="https://unpkg.com/graphiql@3.7.1/graphiql.min.js"></script>
<script>
const fetcher = GraphiQL.createFetcher({ url: location.pathname });
const defaultQuery = ` + "`" + `# Search the LBRY index. Ctrl-Enter runs the query.
{
  status { height }
  search(text: "sailing", first: 5, type: STREAM, mediaType: "video") {
    nodes {
      title
      url
      duration
      thumbnail
      channel { name title }
    }
    pageInfo { hasNextPage endCursor }
  }
}
` + "`" + `;
ReactDOM.createRoot(document.getElementById('graphiql'))
  .render(React.createElement(GraphiQL, { fetcher, defaultQuery }));
</script>
</body>
</html>
`
