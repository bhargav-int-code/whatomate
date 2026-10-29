package mcpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func graphFromJSON(t *testing.T, s string) *chatFlowGraph {
	t.Helper()
	var g chatFlowGraph
	require.NoError(t, json.Unmarshal([]byte(s), &g))
	return &g
}

func TestValidateFlowGraph_ValidFlow(t *testing.T) {
	g := graphFromJSON(t, `{"nodes":[
		{"id":"start","type":"start"},
		{"id":"ask","type":"buttons","config":{"body":"Book a court?","buttons":[{"id":"yes","title":"Yes"},{"id":"no","title":"No"}]}},
		{"id":"name","type":"prompt","config":{"body":"Your name?","store_as":"name"}},
		{"id":"bye","type":"end","config":{"message":"Bye"}},
		{"id":"done","type":"message","config":{"message":"Thanks {{name}}"}}],
	"edges":[{"from":"start","to":"ask"},{"from":"ask","to":"name","condition":"button:yes"},
		{"from":"ask","to":"bye","condition":"button:no"},{"from":"name","to":"done"},{"from":"done","to":"bye"}]}`)

	errs, warns := validateFlowGraph(g)
	assert.Empty(t, errs)
	assert.Empty(t, warns)
	assert.Equal(t, "start", g.EntryNode, "the start node becomes the entry node")
	assert.Equal(t, "default", g.Edges[0].Condition, "a missing condition means default")
}

func TestValidateFlowGraph_Errors(t *testing.T) {
	g := graphFromJSON(t, `{"nodes":[
		{"id":"start","type":"start"},
		{"id":"start","type":"message","config":{"message":"dup"}},
		{"id":"m","type":"message"},
		{"id":"x","type":"teleport"},
		{"id":"b","type":"buttons","config":{"body":"Pick","buttons":[{"id":"a","title":"This title is far too long for WhatsApp"}]}},
		{"id":"c","type":"condition","config":{"expression":"age >= "}},
		{"id":"api","type":"api_call","config":{"url":"ftp://x"}},
		{"id":"t","type":"timing","config":{"schedule":[{"day":"funday","enabled":true}]}},
		{"id":"g","type":"goto_flow","config":{"flow_id":"not-a-uuid"}}],
	"edges":[{"from":"start","to":"nowhere"}]}`)

	errs, _ := validateFlowGraph(g)
	joined := strings.Join(errs, "\n")
	for _, want := range []string{
		`node id "start" is used twice`,
		`message node "m" needs config.message`,
		`unknown type "teleport"`,
		"WhatsApp allows 20",
		"expression does not parse",
		"config.url",
		`day "funday"`,
		"config.flow_id",
		"start → nowhere",
	} {
		assert.Contains(t, joined, want)
	}
}

func TestValidateFlowGraph_Warnings(t *testing.T) {
	g := graphFromJSON(t, `{"entry_node":"start","nodes":[
		{"id":"start","type":"start"},
		{"id":"b","type":"buttons","config":{"body":"Pick","buttons":[{"id":"a","title":"A"},{"id":"z","title":"Z"}]}},
		{"id":"c","type":"condition","config":{"expression":"x > 1"}},
		{"id":"orphan","type":"message","config":{"message":"never"}}],
	"edges":[{"from":"start","to":"b"},{"from":"b","to":"c","condition":"button:a"},{"from":"c","to":"b","condition":"maybe"}]}`)

	errs, warns := validateFlowGraph(g)
	require.Empty(t, errs)
	joined := strings.Join(warns, "\n")
	assert.Contains(t, joined, `outcome "button:z"`, "an unrouted button")
	assert.Contains(t, joined, `never produces outcome "maybe"`)
	assert.Contains(t, joined, `node "orphan" can't be reached`)
}

func TestToStoredGraph_LaysOutByDepth(t *testing.T) {
	g := graphFromJSON(t, `{"entry_node":"s","nodes":[
		{"id":"s","type":"start"},{"id":"a","type":"message","config":{"message":"a"}},
		{"id":"b","type":"message","config":{"message":"b"}},{"id":"c","type":"end"}],
	"edges":[{"from":"s","to":"a"},{"from":"s","to":"b"},{"from":"a","to":"c"}]}`)
	_, _ = validateFlowGraph(g)
	stored := toStoredGraph(g)

	assert.Equal(t, 2, stored["version"])
	pos := map[string]map[string]any{}
	for _, n := range stored["nodes"].([]map[string]any) {
		pos[n["id"].(string)] = n["position"].(map[string]any)
	}
	assert.Equal(t, 0.0, pos["s"]["x"])
	assert.Equal(t, 320.0, pos["a"]["x"])
	assert.Equal(t, 320.0, pos["b"]["x"])
	assert.NotEqual(t, pos["a"]["y"], pos["b"]["y"], "siblings get their own rows")
	assert.Equal(t, 640.0, pos["c"]["x"])
}

func TestValidateButtons(t *testing.T) {
	assert.NoError(t, validateButtons([]chatButton{{ID: "1", Title: "One"}, {ID: "2", Title: "Two"}, {ID: "3", Title: "Three"}}))
	assert.Error(t, validateButtons([]chatButton{{ID: "1", Title: "1"}, {ID: "2", Title: "2"}, {ID: "3", Title: "3"}, {ID: "4", Title: "4"}}))
	assert.Error(t, validateButtons([]chatButton{{ID: "1", Title: "Reply"}, {Type: "url", Title: "Site", URL: "https://x.example"}}))
	assert.Error(t, validateButtons([]chatButton{{Title: "No id"}}))
	assert.NoError(t, validateButtons([]chatButton{{Type: "url", Title: "Site", URL: "https://x.example"}}))
}

func TestNewOutboundURLs(t *testing.T) {
	assert.Equal(t, []string{"https://new.example"}, newOutboundURLs([]string{"https://old.example", "https://new.example", "https://new.example"}, []string{"https://old.example"}))
	assert.Empty(t, newOutboundURLs([]string{"https://old.example"}, []string{"https://old.example"}))
}
