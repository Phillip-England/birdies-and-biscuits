# Birdies & Biscuits

A single-binary Go application for turning the maintained member CSV into a guided public golf directory.

## Run locally

Initialize an environment file:

```sh
go run . init
```

This creates `./config/.env` and initializes `./data/main.sqlite`.

Start the server:

```sh
go run . serve
```

Then open **http://localhost:8777**.

Port `8777` is the application's dedicated default port. If a deployment requires a
different listener, override it with `-addr`, for example
`go run . serve -addr :9000`.

To keep either file elsewhere, pass its location explicitly during initialization:

```sh
go run . init -env /path/to/.env -db /path/to/main.sqlite
go run . serve -env /path/to/.env
```

## Environment file

```env
ADMIN_USERNAME=admin
ADMIN_PASSWORD=change-me-now
SESSION_SECRET=<random-secret>
DB_PATH=../data/main.sqlite
```

`DB_PATH` may be relative. Relative paths are resolved from the environment file's directory, so a server deployment can keep the file in `/etc` and the SQLite database beside it or at an absolute path.

## CSV upload

The admin portal is available at `/login`. Uploading a CSV replaces the public directory.

Required columns:

- `FIRST NAME`
- `LAST NAME`
- `ROLE IN CFA`
- `CITY`
- `STATE`
- `HANDICAP`
- `HOME COURSE`
- `GUEST FEE`
- `BIO`
- `EMAIL`
- `PHONE`

Email and phone are validated during import. Handicap, home course, guest fee, and bio are intentionally stored as descriptive text because the spreadsheet allows ranges and narrative course details.
