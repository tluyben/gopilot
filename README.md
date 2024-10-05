# AI-Assisted Git Workflow

This project implements an AI-assisted Git workflow tool that leverages OpenAI's language models to automate various aspects of Git-based development, including branch naming, code changes, and commit message generation.

## Features

- Automatic generation of Git branch names based on user prompts
- AI-assisted code changes using OpenAI's language models
- Automated commit message generation
- Dependency management (auto-updating go.mod and running go get)
- Customizable prompts for different AI interactions
- Embedded default prompts with the option to override
- Interactive prompt input mode
- Automatic formatting of Go files using goimports
- Option to automatically merge changes into main and delete the feature branch

## Prerequisites

- Go 1.21 or later
- Git
- OpenAI API key
- goimports (automatically installed during setup)

## Installation

1. Clone the repository:

   ```
   git clone https://github.com/tluyben/gopilot.git
   cd gopilot
   ```

2. Install dependencies:

   ```
   make deps
   ```

3. Set up environment variables:
   Create a `.env` file in the project root and add the following:
   ```
   OR_BASE=https://api.openai.com/v1
   OR_TOKEN=your_openai_api_key
   OR_LOW=gpt-3.5-turbo
   OR_HIGH=gpt-4
   ```
   Replace `your_openai_api_key` with your actual OpenAI API key.

## Build

To build the project, run:

```
make build
```

This will create an executable named `gopilot` in the project root.

## Install

To install the binary in /usr/local/bin, run:

```
make install
```

This will build the project and copy the binary to /usr/local/bin.

## Usage

To use the AI-assisted Git workflow tool, run:

```
gopilot -prompt "Your task description here"
```

Additional flags:

- `-files`: Comma-separated list of files to process (default: all *.go, Makefile, *.txt, *.md)
- `-branchprompt`: File containing custom branch name prompt
- `-changesprompt`: File containing custom changes prompt
- `-commitmsgprompt`: File containing custom commit message prompt
- `-inter`: Use interactive prompt mode
- `-merge`: Merge changes into main and delete the branch

Example:

```
gopilot -prompt "Add error handling to database operations" -files "db.go,main.go" -branchprompt custom_branch_prompt.txt -merge
```

To use the interactive prompt mode:

```
gopilot -inter
```

In interactive mode, you can enter a multi-line prompt. Press Ctrl+D (Unix) or Ctrl+Z (Windows) on a new line to finish entering the prompt.

## Customizing Prompts

You can customize the prompts used for AI interactions by creating your own prompt files and specifying them using the appropriate flags. The default prompts are located in the `prompts` directory:

- `branch_name.txt`: Prompt for generating branch names
- `changes.txt`: Prompt for generating code changes
- `commit_message.txt`: Prompt for generating commit messages

To use a custom prompt, create a new text file with your desired prompt and pass it to the program using the appropriate flag.

## How It Works

1. The tool checks if the installed Go version is 1.21 or higher.
2. It generates a new branch name based on your prompt.
3. It creates and checks out the new branch.
4. It uses the OpenAI API to generate code changes based on your prompt and the current project files.
5. The changes are applied to the project files.
6. Dependencies are updated if necessary (go.mod is synced with imports).
7. Go files are formatted using goimports.
8. The project is built using `make build`.
9. If the build succeeds, changes are committed with an AI-generated commit message.
10. If the `-merge` flag is used, the changes are merged into main, pushed, and the feature branch is deleted.

## Contributing

Contributions are welcome! Please feel free to submit a Pull Request.

## License

This project is licensed under the MIT License - see the [LICENSE](LICENSE) file for details.