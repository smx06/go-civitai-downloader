# Go Civitai Downloader

## Overview

This is a command-line tool written in Go to download models from Civitai.com based on specified criteria. It features a two-phase download process (metadata scan + confirmation), concurrent downloads, local database tracking, file verification, and flexible configuration via a file and command-line flags.

## Features

*   **Criteria-Based Downloading:** Fetch models using filters like type, base model, NSFW status, query terms, tags, usernames, etc.
*   **Two-Phase Download:**
    1.  Scans the API based on criteria, checks against the local database, and identifies files *to be* downloaded.
    2.  Presents a summary (file count, total size) and asks for user confirmation before starting downloads.
*   **Concurrent Downloads:** Downloads multiple files simultaneously (configurable concurrency level) for faster fetching.
*   **Local Database:** Uses a Bitcask key/value store (default: `civitai_download_db`) to track successfully downloaded files (keyed by **Model Version ID**, e.g., `v_12345`), preventing redownloads and storing status (`Pending`, `Downloaded`, `Error`).
*   **Gzip Compression:** Database entries are compressed using gzip for reduced storage space.
*   **Database Management Commands:**
    *   `db view`: List entries recorded in the database, including their **status** and **version ID key**.
    *   `db verify`: Check if files recorded in the database exist on disk and optionally verify their hashes. Includes status in log messages.
    *   `db search [QUERY]`: Search database entries by model name, showing **status** and **version ID key**. *(Assumes search command exists/will be updated)*
    *   `db redownload [VERSION_ID]`: Attempt to redownload a specific file using its **Model Version ID**.
*   **Metadata Saving:** Optionally saves a `.json` file containing model/version/file metadata alongside each downloaded file.
*   **Configuration File:** Uses `config.toml` for persistent settings.
*   **Command-Line Flags:** Allows overriding most configuration settings via CLI flags.
*   **Robust API Interaction:** Handles API rate limiting (429) with exponential backoff, uses cursor pagination for deep results, and logs API interactions optionally to `api.log`.
*   **Error Handling:** Includes specific error types for API and download issues.
*   **Structured Logging:** Uses Logrus for leveled logging (configurable via flags).
*   **Interactive Progress:** Uses uilive to show concurrent download progress.

## Caveats

Civitai is a beast of its own, especially the API. There are some things to note:

* The api information returned sometimes is inaccurate, hash values can sometimes be incorrect, or required fields for this app to function are missing.
* At the moment, this application will only download the latest version of a model.
* I've tested this fine downloading all WAN Video LORAs, but I can't guarantee it will work for all model categories.
* Sometimes .tmp files are left over, probably due to failed hash or downloads.

## Building

1.  **Clone the repository:**
    ```bash
    git clone https://github.com/dreamfast/go-civitai-downloader.git
    cd go-civitai-downloader
    ```

2.  **Build using Make (Recommended):**
    A `Makefile` is provided for convenience.
    ```bash
    make build
    ```
    This creates the `civitai-downloader` binary in the project root.

3.  **Build using Go:**
    ```bash
    go build -o civitai-downloader ./cmd/civitai-downloader
    ```

*   **Clean build artifacts:**
    ```bash
    make clean
    ```

## Configuration (`config.toml`)

The application uses a `config.toml` file (default location in the same directory as the executable) for settings. You can specify a different path using the `--config` flag.

| Option                | Type       | Default                                | Description                                                                                             |
| :-------------------- | :--------- | :------------------------------------- | :------------------------------------------------------------------------------------------------------ |
| `ApiKey`              | `string`   | `""`                                   | Your Civitai API Key (optional, but recommended for higher rate limits).                                |
| `SavePath`            | `string`   | `""`                                   | Root directory where model subdirectories (like `lora/`, `checkpoint/`) will be saved. Required.         |
| `DatabasePath`        | `string`   | `""`                                   | Path to the database file. If empty or relative, it's relative to `SavePath` (e.g., `downloads/civitai_download_db`). |
| `Sort`                | `string`   | `"Most Downloaded"`                    | Default sort order for API queries ("Highest Rated", "Most Downloaded", "Newest").                      |
| `Period`              | `string`   | `"AllTime"`                            | Default time period for sorting ("AllTime", "Year", "Month", "Week", "Day").                            |
| `Limit`               | `int`      | `100`                                  | Default models per API page (1-100).                                                                    |
| `Types`               | `[]string` | `[]`                                   | Default model types to query (e.g., `["Checkpoint", "LORA"]`). Empty means all types.                   |
| `BaseModels`          | `[]string` | `[]`                                   | Default base models to query (e.g., `["SDXL 1.0"]`). Empty means all base models.                    |
| `GetNsfw`             | `bool`     | `false`                                | Default setting for including NSFW models in API queries.                                               |
| `GetOnlyPrimaryModel` | `bool`     | `false`                                | Only download the file marked as "primary" for a model version.                                         |
| `GetPruned`           | `bool`     | `false`                                | For Checkpoint models, only download files marked as "pruned".                                          |
| `GetFp16`             | `bool`     | `false`                                | For Checkpoint models, only download files marked as "fp16".                                            |
| `IgnoreBaseModels`    | `[]string` | `[]`                                   | List of base model strings to ignore (case-insensitive substring match).                                |
| `IgnoreFileNameStrings`| `[]string` | `[]`                                   | List of strings to ignore in filenames (case-insensitive substring match).                              |
| `LogApiRequests`      | `bool`     | `false`                                | Log API request/response details to `api.log`.                                                          |
| `SaveMetadata`        | `bool`     | `false`                                | Save a `.json` metadata file next to each downloaded model file.                                        |
| `ApiDelayMs`          | `int`      | `200`                                  | Polite delay (milliseconds) between API metadata requests.                                              |
| `ApiClientTimeoutSec` | `int`      | `60`                                   | Timeout (seconds) for API HTTP client requests.                                                         |
| `DefaultConcurrency`  | `int`      | `4`                                    | Default number of concurrent downloads if `-c` flag is not used.                                        |

### Categories and Config Validation

At the moment the categories for BaseModels must be one of the following "SD 1.4","SD 1.5","SD 1.5 LCM","SD 1.5 Hyper","SD 2.0","SD 2.0 768","SD 2.1","SD 2.1 768","SD 2.1 Unclip","SDXL 0.9","SDXL 1.0","SD 3","SD 3.5","SD 3.5 Medium","SD 3.5 Large","SD 3.5 Large Turbo","Pony","Flux.1 S","Flux.1 D","AuraFlow","SDXL 1.0 LCM","SDXL Distilled","SDXL Turbo","SDXL Lightning","SDXL Hyper","Stable Cascade","SVD","SVD XT","Playground v2","PixArt a","PixArt E","Hunyuan 1","Hunyuan Video","Lumina","Kolors","Illustrious","Mochi","LTXV","CogVideoX","NoobAI","Wan Video","HiDream","Other"

At the moment there isn't very good validation for passing information to the API, so you must ensure that the values, like "LORA" are correct. It is important to note that these values are case sensitive.

If you run any into problems, I suggest to enable the API logging and debug logging to get a better idea of what the problem is.

## Usage

The application is run via the command line.

```bash
./civitai-downloader [global flags] [command] [command flags]
```

**Global Flags:**

*   `--config string`: Path to the configuration file (default \"config.toml\")
*   `--log-level string`: Logging level (debug, info, warn, error) (default \"info\")
*   `--log-format string`: Logging format (text, json) (default \"text\")
*   `--log-api`: Log API requests/responses to `api.log` (overrides config `LogApiRequests`)
*   `--save-path string`: Override the `SavePath` from the config file.
*   `--api-timeout int`: Override `ApiClientTimeoutSec` from config (seconds).
*   `--api-delay int`: Override `ApiDelayMs` from config (milliseconds).

**Commands:**

### `download`

Scans the Civitai API based on filters, asks for confirmation, and then downloads new models.

```bash
./civitai-downloader download [flags]
```

**`download` Flags (override `config.toml`):**

*   `-t, --type strings`: Filter by model type(s) (e.g., Checkpoint, LORA).
*   `-b, --base-model strings`: Filter by base model(s) (e.g., "SD 1.5", SDXL).
*   `--nsfw`: Include NSFW models in query (overrides config `GetNsfw`).
*   `-l, --limit int`: Max models per API page (default 100).
*   `-s, --sort string`: Sort order (default "Most Downloaded").
*   `-p, --period string`: Time period for sorting (default "AllTime").
*   `--primary-only`: Only download primary files (overrides config `GetOnlyPrimaryModel`).
*   `-q, --query string`: Add a search query string.
*   `--tag string`: Filter by specific tag name.
*   `-u, --username string`: Filter by specific username.
*   `--tags strings`: Filter by tags (comma-separated). *(No shorthand)*
*   `--usernames strings`: Filter by usernames (comma-separated). *(No shorthand)*
*   `-m, --model-types strings`: Filter by model types (e.g., Checkpoint, LORA, LoCon).
*   `--pruned`: Only download pruned Checkpoints (overrides config `GetPruned`).
*   `--fp16`: Only download fp16 Checkpoints (overrides config `GetFp16`).
*   `-c, --concurrency int`: Number of concurrent downloads (overrides config `DefaultConcurrency`).
*   `--max-pages int`: Maximum number of API pages to fetch (0 for no limit). *(No shorthand)*
*   `--save-metadata`: Save a `.json` metadata file alongside downloads (overrides config `SaveMetadata`).
*   `-y, --yes`: Skip confirmation prompt before downloading.
*   `--download-meta-only`: Scan, check DB, and save *only* the `.json` metadata files for potential downloads, skipping the actual model file download and confirmation prompt. Useful with `--save-model-info`.
*   `--save-model-info`: During the scan phase, save the *full* JSON data for each model returned by the API to `[SavePath]/model_info/{baseModelSlug}/{modelNameSlug}/{model.ID}.json`. Overwrites existing files for the same model ID.
*   `--save-version-images`: After a model file download succeeds, download the associated preview/example images for that specific version into a `version_images/{versionId}` subdirectory next to the model file.
*   `--save-model-images`: **Requires `--save-model-info`.** When saving the full model info JSON, also attempt to download *all* images associated with *all* versions listed in the model info. Images are saved into `[SavePath]/model_info/{baseModelSlug}/{modelNameSlug}/images/{versionId}/{imageId}.{ext}`.

**Examples:**

*   Download the latest Checkpoint models for SDXL 1.0, increase concurrency, and skip confirmation:
    ```bash
    ./civitai-downloader download --type Checkpoint --base-model "SDXL 1.0" --sort Newest -c 8 -y
    ```

*   Download all LORA models based on the "Wan Video" base model, saving metadata:
    ```bash
    ./civitai-downloader download --type LORA --base-model "Wan Video" --save-metadata
    ```

*   Search for models containing "style" in their name, limit to the first 2 pages of results, and filter for SD 1.5 base models:
    ```bash
    ./civitai-downloader download -q style --limit 100 --max-pages 2 --base-model "SD 1.5"
    ```

### `db`

Parent command for database operations.

#### `db view`

Lists all model file entries recorded in the database, including their **status** and **version ID key**.

```bash
./civitai-downloader db view
```

#### `db verify`

Checks recorded database entries against the filesystem, providing status context.

```bash
./civitai-downloader db verify [--check-hash=true|false]
```

*   `--check-hash`: Perform hash check for existing files (default true).
*   Also checks/creates `.json` metadata files (if main file exists) if `SaveMetadata` is enabled globally (via config or flag).

#### `db redownload`

Attempts to redownload a specific file using its **Model Version ID**.

```bash
./civitai-downloader db redownload <MODEL_VERSION_ID>
```

#### `db search`

Searches database entries for models whose names contain the provided query text, showing **status** and **version ID key**. *(Assumes command exists/is updated)*

```bash
./civitai-downloader db search <MODEL_NAME_QUERY>
```

### `torrent`

Generates BitTorrent `.torrent` files for models previously downloaded and recorded in the database. This requires access to the downloaded files and the database.

```bash
./civitai-downloader torrent --announce <tracker_url> [flags]
```

**`torrent` Flags:**

*   `--announce strings`: **Required.** Tracker announce URL(s). Can be repeated for multiple trackers.
*   `--model-id ints`: Generate torrents only for specific model ID(s). Can be repeated or comma-separated (e.g., `--model-id 123 --model-id 456` or `--model-id 123,456`). Default: all downloaded models in the database.
*   `-o, --output-dir string`: Directory to save generated `.torrent` files. Default: same directory as the model file.
*   `-f, --overwrite`: Overwrite existing `.torrent` files. Default: skip existing files.

**Examples:**

*   Generate torrents for all downloaded models, announcing to two trackers, saving torrents to a specific directory:
    ```bash
    ./civitai-downloader torrent --announce udp://tracker.opentrackr.org:1337/announce --announce udp://tracker.openbittorrent.com:6969/announce -o ./torrents
    ```

*   Generate a torrent only for model ID 12345, overwriting any existing `.torrent` file:
    ```bash
    ./civitai-downloader torrent --announce udp://tracker.opentrackr.org:1337/announce --model-id 12345 -f
    ```

## Project Structure

*   `cmd/civitai-downloader/`: Main application entry point and Cobra command definitions.
*   `internal/`: Internal packages not intended for external use.
    *   `api/`: Civitai API client logic.
    *   `config/`: Configuration loading.
    *   `database/`: Bitcask database interaction wrapper (including Gzip).
    *   `downloader/`: File downloading logic (handles auth, temp files, hash check).
    *   `helpers/`: Utility functions.
    *   `models/`: Struct definitions for config, API responses, database entries.
*   `Makefile`: Build/run/test/clean automation.
*   `config.toml`: Default configuration file.

## Dependencies

*   [github.com/spf13/cobra](https://github.com/spf13/cobra): CLI framework.
*   [github.com/spf13/viper](https://github.com/spf13/viper): Configuration management (used for flag binding).
*   [github.com/BurntSushi/toml](https://github.com/BurntSushi/toml): TOML configuration parsing.
*   [github.com/sirupsen/logrus](https://github.com/sirupsen/logrus): Structured logging.
*   [github.com/gosuri/uilive](https://github.com/gosuri/uilive): Terminal live writer for progress.
*   [git.mills.io/prologic/bitcask](https://git.mills.io/prologic/bitcask): Embedded key/value database.
*   [lukechampine.com/blake3](https://lukechampine.com/blake3): BLAKE3 hashing. 