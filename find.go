package go2ts

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/JakubCzarlinski/go-logging"
	"github.com/JakubCzarlinski/go-pooling"
)

var bufferPool = pooling.CreateBytesBufferPool(256)

// ReadTypes for all files in packagePath
func ReadTypes(packagePath string) (string, error) {
	if rootDirName == "" {
		return "", logging.Bubble(nil, "RootDirName is empty")
	}
	info, err := os.Stat(packagePath)
	if err != nil {
		if os.IsNotExist(err) {
			return "", logging.Bubble(err, "path does not exist")
		}
	}
	if !info.IsDir() {
		return "", logging.Bubble(err, "path is not a directory")
	}
	files, err := os.ReadDir(packagePath)
	if err != nil {
		return "", logging.Bubble(err, "could not read directory")
	}

	fileSet := token.NewFileSet()
	convertBuf := bufferPool.Get()
	defer bufferPool.Reset(convertBuf, struct{}{})

	imports := map[string]string{}
	usedImports := map[string]string{}

	for _, file := range files {
		// Read file
		filePath := file.Name()
		if filepath.Ext(filePath) != ".go" {
			continue
		}

		fullPath := filepath.Join(packagePath, filePath)
		info, err := os.Stat(fullPath)
		if err != nil {
			return "", logging.Bubble(err, "could not stat file")
		}
		if info.IsDir() {
			continue
		}

		astFile, err := parseFile(fullPath, fileSet, file)
		if err != nil {
			return "", logging.Bubble(err, "could not parse file")
		}
		findStructs(astFile, filePath, fileSet, convertBuf, imports, usedImports)
	}

	if len(usedImports) == 0 {
		return convertBuf.String(), nil
	}
	return createPackage(convertBuf, imports, usedImports), nil
}

func addNode(
	filePath string,
	n ast.Node,
	fileSet *token.FileSet,
	convertBuf *pooling.BytesBuffer,
) {
	nodeBuf := bufferPool.Get()
	defer bufferPool.Reset(nodeBuf, struct{}{})
	printer.Fprint(nodeBuf, fileSet, n)
	convertBuf.WriteString(
		fmt.Sprintf("/**\n * [file://./%s](%s)\n */\n", filePath, filePath))
	convertBuf.WriteString("type ")
	convertBuf.Write(nodeBuf.Bytes())
	convertBuf.WriteString("\n\n")
}

func parseFile(
	fullPath string,
	fileSet *token.FileSet,
	file fs.DirEntry,
) (*ast.File, error) {
	source, err := os.ReadFile(fullPath)
	if err != nil {
		return nil, logging.Bubble(err, "could not read file")
	}

	astFile, err := parser.ParseFile(
		fileSet, file.Name(), source, parser.SpuriousErrors)
	if err != nil {
		return nil, logging.Bubble(err, "could not parse Go code")
	}
	return astFile, nil
}

func findStructs(
	astFile *ast.File,
	filePath string,
	fileSet *token.FileSet,
	convertBuf *pooling.BytesBuffer,
	imports map[string]string,
	usedImports map[string]string,
) {
	ast.Inspect(astFile, func(n ast.Node) bool {
		switch x := n.(type) {

		case *ast.ImportSpec:
			var importName string
			if x.Name != nil {
				importName = strings.Trim(x.Name.Name, `"`)
			} else {
				importName = filepath.Base(strings.Trim(x.Path.Value, `"`))
			}
			imports[importName] = strings.Trim(x.Path.Value, `"`)
			return false

		case *ast.TypeSpec:
			addNode(filePath, x, fileSet, convertBuf)
			if x.Type == nil {
				return false
			}

			switch cast := x.Type.(type) {
			case *ast.SelectorExpr:
				usedImports[cast.X.(*ast.Ident).Name] = cast.Sel.Name

			case *ast.StructType:
				for _, field := range cast.Fields.List {
					if selExpr, ok := field.Type.(*ast.SelectorExpr); ok {
						usedImports[selExpr.X.(*ast.Ident).Name] = selExpr.Sel.Name
					}
				}

			}
			return false
		}
		return true
	})
}

func createPackage(
	convertBuf *pooling.BytesBuffer,
	imports map[string]string,
	usedImports map[string]string,
) string {
	importsBuf := bufferPool.Get()
	defer bufferPool.Reset(importsBuf, struct{}{})
	importsBuf.WriteString("package main\n\nimport (\n")
	for key := range usedImports {
		p, ok := imports[key]
		if !ok {
			continue
		}
		if !strings.HasPrefix(p, rootDirName) {
			continue
		}

		base := filepath.Base(p)
		if base == key {
			importsBuf.WriteString(fmt.Sprintf("\t\"%s\"\n", p))
		} else {
			importsBuf.WriteString(fmt.Sprintf("\t%s \"%s\"\n", key, p))
		}
	}

	importsBuf.WriteString(")\n\nfunc main() {\n")
	convertBuf.WriteTo(importsBuf)
	importsBuf.WriteString("}\n")

	return importsBuf.String()
}
