# Meldra

Meldra is an early-stage command-line coding agent. It uses the OpenAI Responses API to chat with you and can list, read, create, and edit files in the directory where it runs.

> **Early stage:** Meldra is experimental. Review every change it makes and avoid running it in directories containing sensitive or irreplaceable files.

## Requirements

- Go 1.25 or newer
- An OpenAI API key

## Quick start

```bash
git clone https://github.com/ruhuang2001/meldra.git
cd meldra
cp .env.example .env
```

Edit `.env` and set `OPENAI_API_KEY`. You can also choose a model with `OPENAI_MODEL`.

Build the binary:

```bash
make build
```

Run it from the project you want to work on so that relative file paths refer to that project:

```bash
cd /path/to/your/project
/path/to/meldra/dist/meldra
```

Or, during development, run it from the Meldra repository:

```bash
go run .
```

Type a request such as:

```text
Read main.go and explain how the agent loop works.
```

Press `Ctrl-C` to exit.

## Configuration

Meldra loads environment variables from `.env` when present.

| Variable | Description |
| --- | --- |
| `OPENAI_API_KEY` | Required API key for the OpenAI API. |
| `OPENAI_BASE_URL` | Optional API base URL. Defaults to the OpenAI API. |
| `OPENAI_MODEL` | Optional model override. |

## Current capabilities

- Read a file
- List files recursively
- Create a text file
- Make an exact, single-match text replacement in a file

## Development

```bash
make check
```

## License

[MIT](LICENSE)
