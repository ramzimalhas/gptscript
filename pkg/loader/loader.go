package loader

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"slices"
	"strings"
	"unicode/utf8"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/gptscript-ai/gptscript/pkg/assemble"
	"github.com/gptscript-ai/gptscript/pkg/builtin"
	"github.com/gptscript-ai/gptscript/pkg/parser"
	"github.com/gptscript-ai/gptscript/pkg/system"
	"github.com/gptscript-ai/gptscript/pkg/types"
	"gopkg.in/yaml.v3"
)

type source struct {
	// Content The content of the source
	Content io.ReadCloser
	// Remote indicates that this file was loaded from a remote source (not local disk)
	Remote bool
	// Path is the path of this source used to find any relative references to this source
	Path string
	// Name is the filename of this source, it does not include the path in it
	Name string
	// Location is a string representation representing the source. It's not assume to
	// be a valid URI or URL, used primarily for display.
	Location string
	// Repo The VCS repo where this tool was found, used to clone and provide the local tool code content
	Repo *types.Repo
}

func (s *source) String() string {
	if s.Path == "" && s.Name == "" {
		return ""
	}
	return s.Path + "/" + s.Name
}

func openFile(path string) (io.ReadCloser, bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil, false, nil
	} else if err != nil {
		return nil, false, err
	}
	return f, true, nil
}

func loadLocal(base *source, name string) (*source, bool, error) {
	path := filepath.Join(base.Path, name)

	content, ok, err := openFile(path)
	if err != nil {
		return nil, false, err
	} else if !ok {
		return nil, false, nil
	}
	log.Debugf("opened %s", path)

	return &source{
		Content:  content,
		Remote:   false,
		Path:     filepath.Dir(path),
		Name:     filepath.Base(path),
		Location: path,
	}, true, nil
}

func loadProgram(data []byte, into *types.Program, targetToolName string) (types.Tool, error) {
	var (
		ext types.Program
	)

	if err := json.Unmarshal(data[len(assemble.Header):], &ext); err != nil {
		return types.Tool{}, err
	}

	into.ToolSet = make(map[string]types.Tool, len(ext.ToolSet))
	for k, v := range ext.ToolSet {
		if builtinTool, ok := builtin.Builtin(k); ok {
			v = builtinTool
		}
		into.ToolSet[k] = v
	}

	tool := into.ToolSet[ext.EntryToolID]
	if targetToolName == "" {
		return tool, nil
	}

	tool, ok := into.ToolSet[tool.LocalTools[strings.ToLower(targetToolName)]]
	if !ok {
		return tool, &types.ErrToolNotFound{
			ToolName: targetToolName,
		}
	}

	return tool, nil
}

func readTool(ctx context.Context, prg *types.Program, base *source, targetToolName string) (types.Tool, error) {
	data, err := io.ReadAll(base.Content)
	if err != nil {
		return types.Tool{}, err
	}
	_ = base.Content.Close()

	if bytes.HasPrefix(data, assemble.Header) {
		return loadProgram(data, prg, targetToolName)
	}

	var tools []types.Tool
	if isOpenAPI(data) {
		if t, err := openapi3.NewLoader().LoadFromData(data); err == nil {
			if base.Remote {
				tools, err = getOpenAPITools(t, base.Location)
			} else {
				tools, err = getOpenAPITools(t, "")
			}
			if err != nil {
				return types.Tool{}, fmt.Errorf("error parsing OpenAPI definition: %w", err)
			}
		}
	}

	if ext := path.Ext(base.Name); len(tools) == 0 && ext != "" && ext != system.Suffix && utf8.Valid(data) {
		tools = []types.Tool{
			{
				Parameters: types.Parameters{
					Name: base.Name,
				},
				Instructions: types.PrintPrefix + "\n" + string(data),
			},
		}
	}

	// If we didn't get any tools from trying to parse it as OpenAPI, try to parse it as a GPTScript
	if len(tools) == 0 {
		tools, err = parser.ParseTools(bytes.NewReader(data), parser.Options{
			AssignGlobals: true,
		})
		if err != nil {
			return types.Tool{}, err
		}
	}

	if len(tools) == 0 {
		return types.Tool{}, fmt.Errorf("no tools found in %s", base)
	}

	var (
		localTools = types.ToolSet{}
		mainTool   types.Tool
	)

	for i, tool := range tools {
		tool.WorkingDir = base.Path
		tool.Source.Location = base.Location
		tool.Source.Repo = base.Repo

		// Probably a better way to come up with an ID
		tool.ID = tool.Source.String()

		if i == 0 {
			mainTool = tool
		}

		if i != 0 && tool.Parameters.Name == "" {
			return types.Tool{}, parser.NewErrLine(tool.Source.Location, tool.Source.LineNo, fmt.Errorf("only the first tool in a file can have no name"))
		}

		if targetToolName != "" && strings.EqualFold(tool.Parameters.Name, targetToolName) {
			mainTool = tool
		}

		if existing, ok := localTools[strings.ToLower(tool.Parameters.Name)]; ok {
			return types.Tool{}, parser.NewErrLine(tool.Source.Location, tool.Source.LineNo,
				fmt.Errorf("duplicate tool name [%s] in %s found at lines %d and %d", tool.Parameters.Name, tool.Source.Location,
					tool.Source.LineNo, existing.Source.LineNo))
		}

		localTools[strings.ToLower(tool.Parameters.Name)] = tool
	}

	return link(ctx, prg, base, mainTool, localTools)
}

func link(ctx context.Context, prg *types.Program, base *source, tool types.Tool, localTools types.ToolSet) (types.Tool, error) {
	if existing, ok := prg.ToolSet[tool.ID]; ok {
		return existing, nil
	}

	tool.ToolMapping = map[string]string{}
	tool.LocalTools = map[string]string{}
	toolNames := map[string]struct{}{}

	// Add now to break circular loops, but later we will update this tool and copy the new
	// tool to the prg.ToolSet
	prg.ToolSet[tool.ID] = tool

	// The below is done in two loops so that local names stay as the tool names
	// and don't get mangled by external references

	for _, targetToolName := range slices.Concat(tool.Parameters.Tools,
		tool.Parameters.Export,
		tool.Parameters.ExportContext,
		tool.Parameters.Context,
		tool.Parameters.Credentials) {
		noArgs, _ := types.SplitArg(targetToolName)
		localTool, ok := localTools[strings.ToLower(noArgs)]
		if ok {
			var linkedTool types.Tool
			if existing, ok := prg.ToolSet[localTool.ID]; ok {
				linkedTool = existing
			} else {
				var err error
				linkedTool, err = link(ctx, prg, base, localTool, localTools)
				if err != nil {
					return types.Tool{}, fmt.Errorf("failed linking %s at %s: %w", targetToolName, base, err)
				}
			}

			tool.ToolMapping[targetToolName] = linkedTool.ID
			toolNames[targetToolName] = struct{}{}
		} else {
			toolName, subTool := SplitToolRef(targetToolName)
			resolvedTool, err := resolve(ctx, prg, base, toolName, subTool)
			if err != nil {
				return types.Tool{}, fmt.Errorf("failed resolving %s at %s: %w", targetToolName, base, err)
			}

			tool.ToolMapping[targetToolName] = resolvedTool.ID
		}
	}

	for _, localTool := range localTools {
		tool.LocalTools[strings.ToLower(localTool.Parameters.Name)] = localTool.ID
	}

	tool = builtin.SetDefaults(tool)
	prg.ToolSet[tool.ID] = tool

	return tool, nil
}

func ProgramFromSource(ctx context.Context, content, subToolName string) (types.Program, error) {
	prg := types.Program{
		ToolSet: types.ToolSet{},
	}
	tool, err := readTool(ctx, &prg, &source{
		Content:  io.NopCloser(strings.NewReader(content)),
		Location: "inline",
	}, subToolName)
	if err != nil {
		return types.Program{}, err
	}
	prg.EntryToolID = tool.ID
	return prg, nil
}

func Program(ctx context.Context, name, subToolName string) (types.Program, error) {
	if subToolName == "" {
		name, subToolName = SplitToolRef(name)
	}
	prg := types.Program{
		Name:    name,
		ToolSet: types.ToolSet{},
	}
	tool, err := resolve(ctx, &prg, &source{}, name, subToolName)
	if err != nil {
		return types.Program{}, err
	}
	prg.EntryToolID = tool.ID
	return prg, nil
}

func resolve(ctx context.Context, prg *types.Program, base *source, name, subTool string) (types.Tool, error) {
	if subTool == "" {
		t, ok := builtin.Builtin(name)
		if ok {
			prg.ToolSet[t.ID] = t
			return t, nil
		}
	}

	s, err := input(ctx, base, name)
	if err != nil {
		return types.Tool{}, err
	}

	return readTool(ctx, prg, s, subTool)
}

func input(ctx context.Context, base *source, name string) (*source, error) {
	if strings.HasPrefix(name, "http://") || strings.HasPrefix(name, "https://") {
		base.Remote = true
	}

	if !base.Remote {
		s, ok, err := loadLocal(base, name)
		if err != nil || ok {
			return s, err
		}
	}

	s, ok, err := loadURL(ctx, base, name)
	if err != nil || ok {
		return s, err
	}

	return nil, fmt.Errorf("can not load tools path=%s name=%s", base.Path, name)
}

func SplitToolRef(targetToolName string) (toolName, subTool string) {
	var (
		fields = strings.Fields(targetToolName)
		idx    = slices.Index(fields, "from")
	)

	defer func() {
		toolName, _ = types.SplitArg(toolName)
	}()

	if idx == -1 {
		return strings.TrimSpace(targetToolName), ""
	}

	return strings.Join(fields[idx+1:], " "),
		strings.Join(fields[:idx], " ")
}

func isOpenAPI(data []byte) bool {
	var fragment struct {
		Paths map[string]any `json:"paths,omitempty"`
	}

	if err := json.Unmarshal(data, &fragment); err != nil {
		if err := yaml.Unmarshal(data, &fragment); err != nil {
			return false
		}
	}
	return len(fragment.Paths) > 0
}
