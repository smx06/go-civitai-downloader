# Define the name for the output binary
BINARY_NAME=civitai-downloader

# Define the path to the main package
MAIN_PKG=./cmd/civitai-downloader

# Define the Go command
GO=go

# Build the application
build:
	@echo "Building $(BINARY_NAME)..."
	$(GO) build -o $(BINARY_NAME) $(MAIN_PKG)
	@echo "$(BINARY_NAME) built successfully."

# Run the application (passes arguments after --)
run: build
	@echo "Running $(BINARY_NAME)..."
	./$(BINARY_NAME) $(ARGS)

# Run tests
test:
	@echo "Running tests..."
	$(GO) test ./... -v

# Clean build artifacts
clean:
	@echo "Cleaning..."
	rm -f $(BINARY_NAME)
	@echo "Clean complete."

# Default target
all: build

# Phony targets (targets that don't represent files)
.PHONY: all build run test clean 