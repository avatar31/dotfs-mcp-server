# graphify graphs for dotfs workspace

## Instatllation

```bash
pipx install graphifyy
```

## Usage

```bash
cp .graphifyignore graphify.config.json <path/to/dotfs-workspace>
cd <path/to/dotfs-workspace>
graphify . --code-only
graphify cluster-only .
```
