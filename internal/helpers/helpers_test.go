package helpers

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go-civitai-download/internal/models" // For models.Hashes
)

func TestConvertToSlug(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  string
	}{
		{"Empty string", "", ""},
		{"Simple string", "Simple Test", "simple_test"},
		{"With colon", "Test: Colon", "test-colon"},
		{"With numbers", "Model V1.5", "model_v1.5"},
		{"Mixed case", "MixedCase Slug", "mixedcase_slug"},
		{"Invalid characters", "File*Name?Is\"Bad!", "filenameisbad"},
		{"Repeated dashes", "double--dash", "double-dash"},
		{"Repeated underscores", "double__underscore", "double_underscore"},
		{"Mixed repeated separators", "mixed-_-separator--test", "mixed-separator-test"},
		{"Leading/trailing spaces (handled by Trim)", "  Leading Trailing  ", "leading_trailing"},
		{"Leading/trailing separators", "-_Leading Trailing_-_", "leading_trailing"},
		{"Already valid", "valid-slug_1.0", "valid-slug_1.0"},
		{"All invalid", "!@#$%^&*()+", ""},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ConvertToSlug(tt.input)
			if got != tt.want {
				t.Errorf("ConvertToSlug(%q) = %q, want %q", tt.input, got, tt.want)
			}
		})
	}
}

func TestBytesToSize(t *testing.T) {
	tests := []struct {
		name  string
		bytes uint64
		want  string
	}{
		{"Zero bytes", 0, "0B"},
		{"Bytes", 500, "500.00B"},
		{"Kilobytes", 1024, "1.00KB"},
		{"Kilobytes fractional", 1536, "1.50KB"},
		{"Megabytes", 1024 * 1024, "1.00MB"},
		{"Megabytes fractional", 1024*1024 + 512*1024, "1.50MB"},
		{"Gigabytes", 1024 * 1024 * 1024, "1.00GB"},
		{"Terabytes", 1024 * 1024 * 1024 * 1024, "1.00TB"},
		{"Large Terabytes", 1536 * 1024 * 1024 * 1024, "1.50TB"},
		// Add edge cases if necessary
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := BytesToSize(tt.bytes)
			if got != tt.want {
				t.Errorf("BytesToSize(%d) = %q, want %q", tt.bytes, got, tt.want)
			}
		})
	}
}

func TestCheckHash(t *testing.T) {
	// Create a temporary directory for test files
	tempDir := t.TempDir()

	// Test file content and its known hashes
	testContent := []byte("this is test content for hashing")
	// Calculate expected hashes (replace with actual known values if preferred)
	expectedBlake3 := "B3C004D66E2A918576F44266A57BBCF854B79ED13D068A6A0EF5156C3CF41B74"
	expectedCRC32 := "4c6b15d9"
	expectedSHA256 := "f7b8f3f1c4c7c3f1d7f1e4e1e5f3f7f9a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0a0" // Placeholder - Recalculate this!
	// Note: You might want to pre-calculate these using external tools (like sha256sum, crc32, b3sum)
	// For SHA256: echo -n "this is test content for hashing" | sha256sum -> e41e304c0e53a1561616a4871f64707701a38342665599694bb3774519a867e7
	expectedSHA256 = "e41e304c0e53a1561616a4871f64707701a38342665599694bb3774519a867e7" // Corrected

	// Create the test file
	testFilePath := filepath.Join(tempDir, "test_hash_file.txt")
	err := os.WriteFile(testFilePath, testContent, 0644)
	if err != nil {
		t.Fatalf("Failed to create test file: %v", err)
	}

	// --- Test Cases ---
	tests := []struct {
		name       string
		filepath   string
		hashes     models.Hashes
		wantResult bool
	}{
		{
			name:       "No file exists",
			filepath:   filepath.Join(tempDir, "nonexistent_file.txt"),
			hashes:     models.Hashes{BLAKE3: expectedBlake3},
			wantResult: false,
		},
		{
			name:       "File exists, BLAKE3 match",
			filepath:   testFilePath,
			hashes:     models.Hashes{BLAKE3: expectedBlake3},
			wantResult: true,
		},
		{
			name:       "File exists, CRC32 match (lowercase api)",
			filepath:   testFilePath,
			hashes:     models.Hashes{CRC32: expectedCRC32}, // Function handles case diff
			wantResult: true,
		},
		{
			name:       "File exists, SHA256 match (uppercase api)",
			filepath:   testFilePath,
			hashes:     models.Hashes{SHA256: strings.ToUpper(expectedSHA256)}, // Function handles case diff
			wantResult: true,
		},
		{
			name:       "File exists, multiple hashes match",
			filepath:   testFilePath,
			hashes:     models.Hashes{BLAKE3: expectedBlake3, CRC32: expectedCRC32, SHA256: expectedSHA256},
			wantResult: true,
		},
		{
			name:       "File exists, one hash mismatch, one match",
			filepath:   testFilePath,
			hashes:     models.Hashes{BLAKE3: "incorrecthash", CRC32: expectedCRC32},
			wantResult: true, // Should return true if any hash matches
		},
		{
			name:       "File exists, all hashes mismatch",
			filepath:   testFilePath,
			hashes:     models.Hashes{BLAKE3: "incorrect1", CRC32: "incorrect2", SHA256: "incorrect3"},
			wantResult: false,
		},
		{
			name:       "File exists, no hashes provided",
			filepath:   testFilePath,
			hashes:     models.Hashes{},
			wantResult: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotResult := CheckHash(tt.filepath, tt.hashes)
			if gotResult != tt.wantResult {
				t.Errorf("CheckHash(%q, %+v) = %v, want %v", tt.filepath, tt.hashes, gotResult, tt.wantResult)
			}
		})
	}
}

func TestCheckAndMakeDir(t *testing.T) {
	// Create a base temporary directory for this test
	baseTempDir := t.TempDir()

	tests := []struct {
		name       string
		dirToMake  string // Relative to baseTempDir
		wantResult bool
		wantExists bool // Check if the directory should actually exist afterwards
	}{
		{
			name:       "Create simple directory",
			dirToMake:  "new_dir",
			wantResult: true,
			wantExists: true,
		},
		{
			name:       "Create nested directory",
			dirToMake:  filepath.Join("nested", "dir", "to", "create"),
			wantResult: true,
			wantExists: true,
		},
		{
			name:       "Attempt to create directory that is a file",
			dirToMake:  "existing_file.txt",
			wantResult: false, // Should fail because it's a file
			wantExists: false, // Directory should not exist
		},
		{
			name:       "Directory already exists",
			dirToMake:  "already_exists",
			wantResult: true, // Should succeed even if it exists
			wantExists: true,
		},
	}

	// Pre-create structures needed for certain tests
	preExistingDir := filepath.Join(baseTempDir, "already_exists")
	if err := os.Mkdir(preExistingDir, 0755); err != nil {
		t.Fatalf("Failed to pre-create directory %s: %v", preExistingDir, err)
	}
	preExistingFile := filepath.Join(baseTempDir, "existing_file.txt")
	if _, err := os.Create(preExistingFile); err != nil {
		t.Fatalf("Failed to pre-create file %s: %v", preExistingFile, err)
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fullPathToMake := filepath.Join(baseTempDir, tt.dirToMake)
			gotResult := CheckAndMakeDir(fullPathToMake)

			if gotResult != tt.wantResult {
				t.Errorf("CheckAndMakeDir(%q) = %v, want %v", fullPathToMake, gotResult, tt.wantResult)
			}

			// Verify if the directory actually exists or not
			_, err := os.Stat(fullPathToMake)
			gotExists := err == nil

			if gotExists != tt.wantExists {
				if tt.wantExists {
					t.Errorf("CheckAndMakeDir(%q) succeeded (%v) but directory does not exist", fullPathToMake, gotResult)
				} else {
					t.Errorf("CheckAndMakeDir(%q) failed (%v) but directory unexpectedly exists", fullPathToMake, gotResult)
				}
			}

			// Double-check if it's actually a directory (if it should exist)
			if tt.wantExists && gotExists {
				info, _ := os.Stat(fullPathToMake)
				if !info.IsDir() {
					t.Errorf("CheckAndMakeDir(%q) created something, but it's not a directory", fullPathToMake)
				}
			}
		})
	}
}

// TODO: Add tests for CheckAndMakeDir (might need filesystem mocking or cleanup)
