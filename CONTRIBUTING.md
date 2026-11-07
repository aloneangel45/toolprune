# Contributing to ToolPrune

Thank you for your interest in contributing to **ToolPrune**! We welcome bug reports, feature suggestions, documentation updates, and pull requests.

## Development Workflow

### Prerequisites
- **Go 1.22+**
- Git

### Setup
1. Fork the repository on GitHub.
2. Clone your fork locally:
   ```bash
   git clone https://github.com/YOUR_USERNAME/toolprune.git
   cd toolprune
   ```
3. Create a descriptive feature or bugfix branch:
   ```bash
   git checkout -b feat/your-feature-name
   ```

### Running Tests & Linting
Ensure all existing tests pass and your changes adhere to idiomatic Go formatting:
```bash
go test -v ./...
go vet ./...
gofmt -s -w .
```

### Commit Guidelines
We follow standard conventional commits:
- `feat:` for new capabilities
- `fix:` for bug fixes
- `perf:` for performance optimizations
- `docs:` for documentation updates
- `test:` for test additions or updates
- `chore:` for dependency updates and repository maintenance

### Pull Request Guidelines
- Keep pull requests focused on a single change or feature.
- Ensure your branch is rebased on the latest `main`.
- Fill out the Pull Request template completely.
- Verify all CI checks pass.
