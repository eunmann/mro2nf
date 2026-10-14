package cgnf

// nextflowKeyword reports a name main.nf's params block cannot declare,
// quoted or not, by Nextflow's v2 script grammar (nf-lang, the same from
// v26.04.0 to v26.04.7): its paramDeclaration takes an identifier, which
// excludes the words ScriptLexer.g4 reads as keywords and literals, and
// ScriptAstBuilder.checkInvalidVarName refuses the Groovy keywords the lexer
// reads as names, and _.
func nextflowKeyword(name string) bool {
	switch name {
	case "as", "def", "assert", "catch", "else", "enum", "if", "import", "include", "instanceof", "new", "return", "throw", "try",
		"boolean", "byte", "char", "double", "float", "int", "long", "short",
		"true", "false", "null":
		return true
	case "abstract", "break", "case", "class", "const", "continue", "default", "do", "extends", "final", "finally", "for", "goto",
		"implements", "interface", "native", "package", "private", "protected", "public", "static", "super", "switch",
		"synchronized", "this", "throws", "transient", "void", "volatile", "while",
		"_":
		return true
	default:
		return false
	}
}
