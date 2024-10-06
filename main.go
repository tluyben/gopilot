package main

import (
	"bufio"
	"bytes"
	"context"
	"embed"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io"
	"io/ioutil"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"text/template"

	"github.com/joho/godotenv"
	"github.com/sashabaranov/go-openai"
	"golang.org/x/mod/modfile"
)

//go:embed prompts/*.txt
var promptFS embed.FS

type FileContent struct {
	FilePath string `json:"filepath"`
	Content  string `json:"content,omitempty"`
	Delete   bool   `json:"delete,omitempty"`
}

type Config struct {
	OrBase          string
	OrToken         string
	OrLow           string
	OrHigh          string
	Files           string
	Prompt          string
	GitBranch       string
	BranchPrompt    string
	ChangesPrompt   string
	CommitMsgPrompt string
	FixJsonPrompt   string
	ProjectName     string
	Merge           bool
	Remove          bool
	SplitFiles      string
	UnsplitFiles    string
}

type Session struct {
	TotalCost float64
	Requests  int
}

var currentSession Session

type WrappedOpenAIClient struct {
	client *openai.Client
}

func dependenciesNeedUpdate() bool {
	goModContent, err := ioutil.ReadFile("go.mod")
	if err != nil {
		log.Fatal("Error reading go.mod:", err)
	}

	mainContent, err := ioutil.ReadFile("main.go")
	if err != nil {
		log.Fatal("Error reading main.go:", err)
	}

	modFile, err := modfile.Parse("go.mod", goModContent, nil)
	if err != nil {
		log.Fatal("Error parsing go.mod:", err)
	}

	scanner := bufio.NewScanner(bytes.NewReader(mainContent))
	for scanner.Scan() {
		line := scanner.Text()
		if strings.HasPrefix(line, "import") {
			for scanner.Scan() {
				importLine := scanner.Text()
				if importLine == ")" {
					break
				}
				packageName := strings.Trim(importLine, "\t \"")
				if !isPackageInModFile(modFile, packageName) {
					return true
				}
			}
		}
	}

	return false
}

func loadConfig() Config {
	godotenv.Load()

	config := Config{
		OrBase:  os.Getenv("OR_BASE"),
		OrToken: os.Getenv("OR_TOKEN"),
		OrLow:   os.Getenv("OR_LOW"),
		OrHigh:  os.Getenv("OR_HIGH"),
	}

	if config.OrBase == "" {
		config.OrBase = "https://openrouter.ai/api/v1/chat/completions"
	}

	flag.StringVar(&config.Files, "files", "", "Comma-separated list of files to process")
	flag.StringVar(&config.Prompt, "prompt", "", "User prompt for changes")
	flag.StringVar(&config.BranchPrompt, "branchprompt", "", "File containing the branch name prompt")
	flag.StringVar(&config.ChangesPrompt, "changesprompt", "", "File containing the changes prompt")
	flag.StringVar(&config.CommitMsgPrompt, "commitmsgprompt", "", "File containing the commit message prompt")
	flag.StringVar(&config.FixJsonPrompt, "fixjsonprompt", "", "File containing the fix JSON prompt")
	flag.BoolVar(&config.Merge, "merge", false, "Merge changes into main and delete the branch")
	flag.BoolVar(&config.Remove, "rm", false, "Delete the current branch and move back to main branch")
	flag.StringVar(&config.SplitFiles, "split", "", "Comma-separated list of Go files to split into .gopart files")
	flag.StringVar(&config.UnsplitFiles, "unsplit", "", "Comma-separated list of Go files to recreate from .gopart files")

	// Add the new flag for interactive prompt
	interactive := flag.Bool("inter", false, "Use interactive prompt")

	flag.Parse()

	if config.OrBase == "" || config.OrToken == "" || config.OrLow == "" || config.OrHigh == "" {
		log.Fatal("Missing required environment variables")
	}

	// If interactive mode is enabled, read the prompt from stdin
	if *interactive {
		config.Prompt = readInteractivePrompt()
	}

	if config.Merge && config.Prompt == "" {
		currentBranch := getCurrentBranch()
		mergeAndCleanup(currentBranch)
		// exit
		os.Exit(0)
	}

	if config.Remove && config.Prompt == "" {
		currentBranch := getCurrentBranch()
		removeAndCleanup(currentBranch)
		// exit
		os.Exit(0)
	}


	if config.Prompt == "" && !config.Remove && config.SplitFiles == "" && config.UnsplitFiles == "" {
		log.Fatal("Prompt is required")
	}

	// Get the project name from the current directory
	cwd, err := os.Getwd()
	if err != nil {
		log.Fatal("Error getting current working directory:", err)
	}
	config.ProjectName = filepath.Base(cwd)

	// Initialize the current session
	currentSession = Session{TotalCost: 0, Requests: 0}

	return config
}

func main() {
	checkGoVersion()
	config := loadConfig()

	if config.SplitFiles != "" {
		splitGoFiles(config.SplitFiles)
		return
	}

	if config.UnsplitFiles != "" {
		unsplitGoFiles(config.UnsplitFiles)
		return
	}

	// Automatically split all *.go files in the root directory
	goFiles, err := filepath.Glob("*.go")
	if err != nil {
		log.Fatal("Error finding Go files:", err)
	}
	splitGoFiles(strings.Join(goFiles, ","))

	files := readGoPartFiles("editor")
	branchName := generateBranchName(config, files)
	checkoutBranch(branchName)

	changes := generateChanges(config, files)
	applyChanges(changes)

	// Unsplit files after changes are applied
	unsplitGoFiles(strings.Join(goFiles, ","))

	updateDependencies()

	ensureGoimportsInstalled()
	runGoimports()

	if buildSucceeds() {
		commitChanges(config)
		fmt.Println("Changes applied and committed successfully.")
		showDiff()

		if config.Merge {
			mergeAndCleanup(branchName)
		}
	} else {
		fmt.Println("Build failed. Please fix the issues and try again.")
	}

	// Print session summary
	fmt.Printf("Session summary:\nTotal requests: %d\nTotal cost: $%.2f\n", currentSession.Requests, currentSession.TotalCost)
}

func writeSplitOrder(path string, order []string) error {
	content, err := json.Marshal(order)
	if err != nil {
		return err
	}
	return ioutil.WriteFile(path, content, 0644)
}

func runGoGet() {
	cmd := exec.Command("go", "get", "./...")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		log.Fatalf("Error running go get: %v\n%s", err, stderr.String())
	}
}

func splitGoFile(filename string) {
	// Read the Go file
	content, err := ioutil.ReadFile(filename)
	if err != nil {
		log.Fatalf("Error reading file %s: %v", filename, err)
	}

	// Parse the Go file
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, filename, content, parser.ParseComments)
	if err != nil {
		log.Fatalf("Error parsing file %s: %v", filename, err)
	}

	// Create the editor directory
	baseDir := filepath.Join("editor", strings.TrimSuffix(filename, ".go"))
	err = os.MkdirAll(baseDir, 0755)
	if err != nil {
		log.Fatalf("Error creating directory %s: %v", baseDir, err)
	}

	// Extract package declaration and imports
	var packageAndImports strings.Builder
	packageAndImports.WriteString(fmt.Sprintf("package %s\n\n", f.Name.Name))

	for _, decl := range f.Decls {
		if gen, ok := decl.(*ast.GenDecl); ok && gen.Tok == token.IMPORT {
			packageAndImports.WriteString(string(content[gen.Pos()-1:gen.End()]) + "\n")
		}
	}

	var varsAndStructs strings.Builder
	functions := make(map[string]string)

	// Extract vars, structs, and functions
	for _, decl := range f.Decls {
		switch d := decl.(type) {
		case *ast.GenDecl:
			if d.Tok == token.VAR || d.Tok == token.TYPE {
				// Check for //go:embed directives
				if d.Doc != nil {
					for _, c := range d.Doc.List {
						if strings.HasPrefix(c.Text, "//go:embed") {
							varsAndStructs.WriteString(c.Text + "\n")
						}
					}
				}
				varsAndStructs.WriteString(string(content[d.Pos()-1:d.End()]) + "\n\n")
			}
		case *ast.FuncDecl:
			name := d.Name.Name
			functions[name] = string(content[d.Pos()-1:d.End()]) + "\n"
		}
	}

	// Write .gopart files
	writeGopart(baseDir, "imports.gopart", packageAndImports.String())
	writeGopart(baseDir, "varsandstructs.gopart", varsAndStructs.String())

	// Create and write splitorder.json
	splitOrder := []string{"imports.gopart", "varsandstructs.gopart"}
	for name := range functions {
		splitOrder = append(splitOrder, name+".gopart")
		writeGopart(baseDir, name+".gopart", functions[name])
	}

	splitOrderJSON, err := json.Marshal(splitOrder)
	if err != nil {
		log.Fatalf("Error marshaling split order: %v", err)
	}
	writeGopart(baseDir, "splitorder.json", string(splitOrderJSON))

	fmt.Printf("Split %s into .gopart files in %s\n", filename, baseDir)
}

func writeGopart(dir, filename, content string) {
	path := filepath.Join(dir, filename)
	err := ioutil.WriteFile(path, []byte(content), 0644)
	if err != nil {
		log.Fatalf("Error writing file %s: %v", path, err)
	}
}

func splitGoFiles(fileList string) {
	files := strings.Split(fileList, ",")
	for _, file := range files {
		splitGoFile(strings.TrimSpace(file))
	}
}

func calculateCost(response openai.ChatCompletionResponse, inputTokens int) float64 {
	var outputTokens int
	var cost float64

	// Count output tokens
	outputTokens = len(response.Choices[0].Message.Content)

	// Calculate cost based on model
	switch response.Model {
	case "claude-3-sonnet-20240229":
		cost = (float64(inputTokens) * 3.0 / 1e6) + (float64(outputTokens) * 15.0 / 1e6)
	case "claude-3-haiku-20240307":
		cost = (float64(inputTokens) * 0.25 / 1e6) + (float64(outputTokens) * 1.25 / 1e6)
	default:
		// Default cost calculation if model is not recognized
		cost = 0.01
	}

	return cost
}

func commitChanges(config Config) {
	commitMsg := generateCommitMessage(config)
	cmd := exec.Command("git", "add", ".")
	err := cmd.Run()
	if err != nil {
		log.Fatal(err)
	}

	cmd = exec.Command("git", "commit", "-m", commitMsg)
	err = cmd.Run()
	if err != nil {
		log.Fatal(err)
	}
}

func updateSplitOrder(order []string, newFile, insertionPoint string, insertBefore bool) []string {
	for i, file := range order {
		if file == insertionPoint+".gopart" {
			if insertBefore {
				return append(order[:i], append([]string{newFile}, order[i:]...)...)
			}
			return append(order[:i+1], append([]string{newFile}, order[i+1:]...)...)
		}
	}
	return append(order, newFile)
}

func runGoimports() {
	goimportsPath, err := findGoimports()
	if err != nil {
		log.Println("Warning: Could not find goimports:", err)
		return
	}

	cmd := exec.Command(goimportsPath, "-w", ".")
	err = cmd.Run()
	if err != nil {
		log.Println("Warning: Failed to run goimports:", err)
	}
}

func showDiff() {
	cmd := exec.Command("git", "diff", "--cached", "main")
	output, err := cmd.Output()
	if err != nil {
		log.Printf("Error getting diff: %v", err)
		return
	}

	fmt.Println("\nChanges made:")
	fmt.Println(string(output))
}

func unsplitGoFile(filename string) {
	baseDir := filepath.Join("editor", strings.TrimSuffix(filename, ".go"))

	// Check if the directory exists
	if _, err := os.Stat(baseDir); os.IsNotExist(err) {
		log.Fatalf("Directory %s does not exist. Make sure you've split the file first.", baseDir)
	}

	// Read splitorder.json
	splitOrderJSON, err := ioutil.ReadFile(filepath.Join(baseDir, "splitorder.json"))
	if err != nil {
		log.Fatalf("Error reading splitorder.json: %v", err)
	}

	var splitOrder []string
	err = json.Unmarshal(splitOrderJSON, &splitOrder)
	if err != nil {
		log.Fatalf("Error unmarshaling split order: %v", err)
	}

	// Read .gopart files in the order specified by splitorder.json
	var parts []string
	for _, partFile := range splitOrder {
		content, err := ioutil.ReadFile(filepath.Join(baseDir, partFile))
		if err != nil {
			log.Fatalf("Error reading file %s: %v", partFile, err)
		}
		parts = append(parts, string(content))
	}

	// Combine the parts
	combinedContent := strings.Join(parts, "\n\n")

	// Write the combined content to the original .go file
	err = ioutil.WriteFile(filename, []byte(combinedContent), 0644)
	if err != nil {
		log.Fatalf("Error writing file %s: %v", filename, err)
	}

	fmt.Printf("Recreated %s from .gopart files in %s\n", filename, baseDir)
}

func (w *WrappedOpenAIClient) CreateChatCompletionStream(ctx context.Context, request openai.ChatCompletionRequest) (*openai.ChatCompletionStream, error) {
	stream, err := w.client.CreateChatCompletionStream(ctx, request)
	if err == nil {
		currentSession.Requests++
		// Note: Cost calculation for streaming responses might need to be handled differently
	}
	return stream, err
}

func readInteractivePrompt() string {
	fmt.Println("Enter your prompt (press Ctrl+D or Ctrl+Z on a new line to finish):")
	scanner := bufio.NewScanner(os.Stdin)
	var lines []string
	for scanner.Scan() {
		lines = append(lines, scanner.Text())
	}
	if err := scanner.Err(); err != nil {
		log.Fatal("Error reading interactive prompt:", err)
	}
	return strings.Join(lines, "\n")
}

func removeAndCleanup(branchName string) {
	if (branchName == "main") {
		log.Fatal("Cannot delete main branch.")
	}
	cmd := exec.Command("git", "stash")
	output, err := cmd.CombinedOutput()
	if err != nil {
		log.Fatal("Error while stashing changes:", string(output), err)
	}
	// Checkout main
	cmd = exec.Command("git", "checkout", "main")
	output, err = cmd.CombinedOutput()
	if err != nil {
		log.Fatal("Error checking out main branch:", string(output), err)
	}

	// Delete the branch
	cmd = exec.Command("git", "branch", "-D", branchName)
	err = cmd.Run()
	if err != nil {
		log.Fatal("Error deleting branch:", err)
	}

	fmt.Printf("Branch %s deleted and moved back to main branch.\n", branchName)
}

func updateDependencies() {
	if dependenciesNeedUpdate() {
		fmt.Println("Updating dependencies...")
		runGoGet()
		runGoModTidy()
	} else {
		fmt.Println("Dependencies are up to date.")
	}
}

func buildSucceeds() bool {
	cmd := exec.Command("make", "build")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		fmt.Println("Build failed. Error output:")
		fmt.Println(stderr.String())
		return false
	}

	return true
}

func (w *WrappedOpenAIClient) CreateChatCompletion(ctx context.Context, request openai.ChatCompletionRequest) (openai.ChatCompletionResponse, error) {
	response, err := w.client.CreateChatCompletion(ctx, request)
	if err == nil {
		currentSession.Requests++
		inputTokens := len(request.Messages[len(request.Messages)-1].Content)
		currentSession.TotalCost += calculateCost(response, inputTokens)
	}
	return response, err
}

func unsplitGoFiles(fileList string) {
	files := strings.Split(fileList, ",")
	for _, file := range files {
		unsplitGoFile(strings.TrimSpace(file))
	}
}

func getPromptContent(userFile, defaultFile string) string {
	if userFile != "" {
		content, err := ioutil.ReadFile(userFile)
		if err == nil {
			return string(content)
		}
		log.Printf("Warning: Could not read user-provided prompt file %s. Using default. in getPromptContent", userFile)
	}

	content, err := promptFS.ReadFile(defaultFile)
	if err != nil {
		log.Fatalf("Error reading default prompt file %s: %v  in getPromptContent", defaultFile, err)
	}
	return string(content)
}

func applyChanges(changes []FileContent) {
	for _, change := range changes {
		if change.Delete {
			err := os.Remove(change.FilePath)
			if err != nil {
				log.Printf("Error deleting file %s: %v", change.FilePath, err)
			} else {
				fmt.Printf("Deleted file: %s\n", change.FilePath)
			}
		} else {
			err := ioutil.WriteFile(change.FilePath, []byte(change.Content), 0644)
			if err != nil {
				log.Printf("Error writing file %s: %v", change.FilePath, err)
			} else {
				fmt.Printf("Updated file: %s\n", change.FilePath)
			}
		}
	}
}

func getInsertionPoint(content string) (bool, string) {
	lines := strings.Split(content, "\n")
	for _, line := range lines {
		if strings.HasPrefix(line, "// insert-before: ") {
			return true, strings.TrimPrefix(line, "// insert-before: ")
		}
		if strings.HasPrefix(line, "// insert-after: ") {
			return false, strings.TrimPrefix(line, "// insert-after: ")
		}
	}
	return false, ""
}

func readFiles(fileList string) []FileContent {
	var files []FileContent

	if fileList == "" {
		// Default file patterns
		patterns := []string{"*.go", "Makefile", "*.txt", "*.md"}
		for _, pattern := range patterns {
			matches, err := filepath.Glob(pattern)
			if err != nil {
				log.Printf("Error globbing pattern %s: %v", pattern, err)
				continue
			}
			for _, match := range matches {
				if !strings.Contains(match, ".git") {
					addFileContent(&files, match)
				}
			}
		}
	} else {
		for _, file := range strings.Split(fileList, ",") {
			addFileContent(&files, strings.TrimSpace(file))
		}
	}

	return files
}

func checkGoVersion() {
	constraint := "1.21"
	currentVersion := runtime.Version()
	if !strings.HasPrefix(currentVersion, "go1.") {
		log.Fatalf("Unsupported Go version: %s. Go 1.21 or higher is required.", currentVersion)
	}
	versionNumber := strings.TrimPrefix(currentVersion, "go")
	if versionNumber < constraint {
		log.Fatalf("Go version %s is required, but you have %s. Please upgrade your Go installation.", constraint, currentVersion)
	}
}

func createOpenAIClient(config Config) *WrappedOpenAIClient {
	_config := openai.DefaultConfig(config.OrToken)
	_config.BaseURL = config.OrBase
	client := openai.NewClientWithConfig(_config)

	return &WrappedOpenAIClient{
		client: client,
	}
}

func ensureGoimportsInstalled() {
	cmd := exec.Command("go", "install", "golang.org/x/tools/cmd/goimports@latest")
	err := cmd.Run()
	if err != nil {
		log.Fatal("Failed to install goimports:", err)
	}
}

func findGoimports() (string, error) {
	// Check in ~/go/bin/ (common location on macOS and Linux)
	homeDir, err := os.UserHomeDir()
	if err == nil {
		path := filepath.Join(homeDir, "go", "bin", "goimports")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// Check in GOPATH/bin
	gopath := os.Getenv("GOPATH")
	if gopath != "" {
		path := filepath.Join(gopath, "bin", "goimports")
		if _, err := os.Stat(path); err == nil {
			return path, nil
		}
	}

	// Check in PATH
	return exec.LookPath("goimports")
}

func getCurrentBranch() string {
	cmd := exec.Command("git", "rev-parse", "--abbrev-ref", "HEAD")
	out, err := cmd.Output()
	if err != nil {
		log.Fatal(err)
	}
	return strings.TrimSpace(string(out))
}

func generateAdditionalChanges(config Config, existingChanges []FileContent, remainingContent string) []FileContent {
	client := createOpenAIClient(config)

	promptContent := getPromptContent(config.ChangesPrompt, "prompts/changes.txt")
	tmpl, err := template.New("changes").Parse(promptContent)
	if err != nil {
		log.Fatal(err, "in generateAdditionalChanges: template parsing")
	}

	existingChangesJSON, _ := json.Marshal(existingChanges)
	var promptBuffer bytes.Buffer
	err = tmpl.Execute(&promptBuffer, map[string]string{
		"Prompt":           config.Prompt,
		"ExistingChanges":  string(existingChangesJSON),
		"RemainingContent": remainingContent,
		"ProjectName":      config.ProjectName,
	})
	if err != nil {
		log.Fatal(err, "in generateAdditionalChanges: template execution")
	}

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: config.OrHigh,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: promptBuffer.String(),
				},
			},
		},
	)

	if err != nil {
		log.Fatal(err, "in generateAdditionalChanges")
	}

	fmt.Println("additional changes suggestion: ", resp.Choices[0].Message.Content)

	return parseChanges(config, resp.Choices[0].Message.Content)
}

func generateBranchName(config Config, files []FileContent) string {
	client := createOpenAIClient(config)
	currentBranch := getCurrentBranch()

	promptContent := getPromptContent(config.BranchPrompt, "prompts/branch_name.txt")
	tmpl, err := template.New("branch").Parse(promptContent)
	if err != nil {
		log.Fatal(err, "in generateBranchName: template parsing")
	}

	var promptBuffer bytes.Buffer
	err = tmpl.Execute(&promptBuffer, map[string]string{
		"Prompt":        config.Prompt,
		"CurrentBranch": currentBranch,
	})
	if err != nil {
		log.Fatal(err)
	}

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: config.OrLow,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: promptBuffer.String(),
				},
			},
		},
	)

	if err != nil {
		log.Fatal(err, resp, "in generateBranchName")
	}

	fmt.Println("branch name suggestion: ", resp.Choices[0].Message.Content)

	return strings.TrimSpace(resp.Choices[0].Message.Content)
}

func generateChanges(config Config, files []FileContent) []FileContent {
	fmt.Println("Generating changes...")

	client := createOpenAIClient(config)

	promptContent := getPromptContent(config.ChangesPrompt, "prompts/changes.txt")
	tmpl, err := template.New("changes").Parse(promptContent)
	if err != nil {
		log.Fatal(err, "in generateChanges: template parsing")
	}

	filesJSON, _ := json.Marshal(files)
	var promptBuffer bytes.Buffer
	err = tmpl.Execute(&promptBuffer, map[string]string{
		"Prompt":      config.Prompt,
		"Files":       string(filesJSON),
		"ProjectName": config.ProjectName,
	})
	if err != nil {
		log.Fatal(err, "in generateChanges: template execution")
	}

	stream, err := client.CreateChatCompletionStream(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: config.OrHigh,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: promptBuffer.String(),
				},
			},
		},
	)

	if err != nil {
		log.Fatal(err, "in generateChanges")
	}
	defer stream.Close()

	var fullResponse strings.Builder
	for {
		response, err := stream.Recv()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			log.Fatal("Stream error:", err)
		}
		fullResponse.WriteString(response.Choices[0].Delta.Content)
		fmt.Print(response.Choices[0].Delta.Content)
	}

	fmt.Println("\nRaw changes suggestion:", fullResponse.String())

	return parseChanges(config, fullResponse.String())
}

func mergeAndCleanup(branchName string) {

	if (branchName == "main") {
		log.Fatal("Cannot delete main branch.")
	}
	// Check for uncommitted changes
	cmd := exec.Command("git", "status", "--porcelain")
	output, err := cmd.Output()
	if err != nil {
		log.Fatal("Error checking git status:", err)
	}

	if len(output) > 0 {
		// There are uncommitted changes
		fmt.Println("Uncommitted changes detected. Committing before merge...")

		// Stage all changes
		cmd = exec.Command("git", "add", "-A")
		err = cmd.Run()
		if err != nil {
			log.Fatal("Error staging changes:", err)
		}

		// Generate commit message
		config := loadConfig()
		commitMsg := generateCommitMessage(config)

		// Commit changes
		cmd = exec.Command("git", "commit", "-m", commitMsg)
		err = cmd.Run()
		if err != nil {
			log.Fatal("Error committing changes:", err)
		}

		fmt.Println("Uncommitted changes have been committed.")
	}

	// Checkout main
	cmd = exec.Command("git", "checkout", "main")
	output, err = cmd.CombinedOutput()
	if err != nil {
		log.Fatal("Error checking out main branch:", string(output), err)
	}

	// Merge the branch
	cmd = exec.Command("git", "merge", branchName)
	err = cmd.Run()
	if err != nil {
		log.Fatal("Error merging branch:", err)
	}

	// Push changes
	cmd = exec.Command("git", "push")
	err = cmd.Run()
	if err != nil {
		log.Fatal("Error pushing changes:", err)
	}

	// Delete the branch
	cmd = exec.Command("git", "branch", "-D", branchName)
	err = cmd.Run()
	if err != nil {
		log.Fatal("Error deleting branch:", err)
	}

	fmt.Printf("Branch %s merged into main, pushed, and deleted.\n", branchName)
}

func readGoPartFiles(dir string) []FileContent {
	var files []FileContent

	err := filepath.Walk(dir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if !info.IsDir() && filepath.Ext(path) == ".gopart" {
			content, err := ioutil.ReadFile(path)
			if err != nil {
				return err
			}
			files = append(files, FileContent{FilePath: path, Content: string(content)})
		}
		return nil
	})

	if err != nil {
		log.Fatal("Error reading .gopart files:", err)
	}

	return files
}

func isPackageInModFile(modFile *modfile.File, packageName string) bool {
	for _, req := range modFile.Require {
		if strings.HasPrefix(packageName, req.Mod.Path) {
			return true
		}
	}
	return false
}

func checkoutBranch(branchName string) {
	cmd := exec.Command("git", "checkout", "-b", branchName)
	err := cmd.Run()
	if err != nil {
		fmt.Println(err, cmd, " in checkoutBranch, not fatal")
		cmd := exec.Command("git", "checkout", branchName)
		cmd.Run()
		//log.Fatal(err, cmd, " in checkoutBranch")
	}
}

func generateCommitMessage(config Config) string {
	client := createOpenAIClient(config)

	promptContent := getPromptContent(config.CommitMsgPrompt, "prompts/commit_message.txt")
	tmpl, err := template.New("commit").Parse(promptContent)
	if err != nil {
		log.Fatal(err)
	}

	var promptBuffer bytes.Buffer
	err = tmpl.Execute(&promptBuffer, map[string]string{
		"Prompt":      config.Prompt,
		"ProjectName": config.ProjectName,
	})
	if err != nil {
		log.Fatal(err)
	}

	resp, err := client.CreateChatCompletion(
		context.Background(),
		openai.ChatCompletionRequest{
			Model: config.OrLow,
			Messages: []openai.ChatCompletionMessage{
				{
					Role:    openai.ChatMessageRoleUser,
					Content: promptBuffer.String(),
				},
			},
		},
	)

	if err != nil {
		log.Fatal(err)
	}

	fmt.Println("commit message suggestion: ", resp.Choices[0].Message.Content)

	return strings.TrimSpace(resp.Choices[0].Message.Content)
}

func getFirstKeyword(s string) string {
	lines := strings.Split(s, "\n")
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if strings.HasPrefix(trimmed, "//go:embed") {
			continue // Skip //go:embed lines when determining the first keyword
		}
		words := strings.Fields(trimmed)
		if len(words) > 0 {
			return words[0]
		}
	}
	return ""
}

func parseChanges(config Config, rawChanges string) []FileContent {
	var changes []FileContent

	// first let's just try the naive way
	err := json.Unmarshal([]byte(rawChanges), &changes)
	if err == nil {
		return changes
	}

	start := strings.Index(rawChanges, "[")
	if start == -1 {
		return changes
	}

	rawJSON := rawChanges[start:]
	var validJSON strings.Builder
	depth := 0
	inString := false
	escaped := false
	objectStarted := false

	for _, char := range rawJSON {
		if inString {
			if !escaped && char == '"' {
				inString = false
			}
			escaped = char == '\\' && !escaped
		} else if char == '"' && !escaped {
			inString = true
		} else if char == '{' && !inString {
			depth++
			objectStarted = true
		} else if char == '}' && !inString {
			depth--
			if depth == 0 && objectStarted {
				validJSON.WriteRune(char)
				break
			}
		}

		if objectStarted {
			validJSON.WriteRune(char)
		}
	}

	validJSONString := validJSON.String()
	if validJSONString == "" {
		return changes
	}

	err = json.Unmarshal([]byte(validJSONString), &changes)
	if err != nil {
		fmt.Println("Error unmarshalling changes:", err)
		return changes
	}

	// If there are more changes to process, recursively call generateChanges
	if len(validJSONString) < len(rawJSON) {
		additionalChanges := generateAdditionalChanges(config, changes, rawJSON[len(validJSONString):])
		changes = append(changes, additionalChanges...)
	}

	// Process insert-before and insert-after
	for i, change := range changes {
		var insertionPoint string
		var insertBefore bool

		if insertBefore, insertionPoint = getInsertionPoint(change.Content); insertBefore || insertionPoint != "" {
			baseDir := filepath.Dir(change.FilePath)
			splitOrderPath := filepath.Join(baseDir, "splitorder.json")
			splitOrder, err := readSplitOrder(splitOrderPath)
			if err != nil {
				fmt.Printf("Error reading split order for %s: %v\n", change.FilePath, err)
				continue
			}

			newOrder := updateSplitOrder(splitOrder, filepath.Base(change.FilePath), insertionPoint, insertBefore)
			writeSplitOrder(splitOrderPath, newOrder)

			// Remove the insertion point from the content
			changes[i].Content = removeInsertionPoint(change.Content)
		}
	}

	return changes
}

func readSplitOrder(path string) ([]string, error) {
	content, err := ioutil.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var order []string
	err = json.Unmarshal(content, &order)
	return order, err
}

func removeInsertionPoint(content string) string {
	lines := strings.Split(content, "\n")
	var newLines []string
	for _, line := range lines {
		if !strings.HasPrefix(line, "// insert-before: ") && !strings.HasPrefix(line, "// insert-after: ") {
			newLines = append(newLines, line)
		}
	}
	return strings.Join(newLines, "\n")
}

func runGoModTidy() {
	cmd := exec.Command("go", "mod", "tidy")
	var stderr bytes.Buffer
	cmd.Stderr = &stderr

	err := cmd.Run()
	if err != nil {
		log.Fatalf("Error running go mod tidy: %v\n%s", err, stderr.String())
	}
}

func addFileContent(files *[]FileContent, path string) {
	if info, err := os.Stat(path); err == nil {
		if info.IsDir() {
			err := filepath.Walk(path, func(subpath string, subinfo os.FileInfo, err error) error {
				if err != nil {
					return err
				}
				if !subinfo.IsDir() && !strings.Contains(subpath, ".git") {
					content, err := ioutil.ReadFile(subpath)
					if err != nil {
						log.Printf("Error reading file %s: %v", subpath, err)
						return nil
					}
					*files = append(*files, FileContent{FilePath: subpath, Content: string(content)})
				}
				return nil
			})
			if err != nil {
				log.Printf("Error walking directory %s: %v", path, err)
			}
		} else {
			content, err := ioutil.ReadFile(path)
			if err != nil {
				log.Printf("Error reading file %s: %v", path, err)
				return
			}
			*files = append(*files, FileContent{FilePath: path, Content: string(content)})
		}
	} else {
		log.Printf("Error accessing file or directory %s: %v", path, err)
	}
}
