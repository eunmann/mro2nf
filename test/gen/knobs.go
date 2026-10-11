package gen

// The generator's knobs. A pct* constant is the percent chance a choice is
// taken; a *Below constant is the upper bound of a choice's band in a roll of
// [0, percent); a max* or min* constant bounds a count. Changing any of them
// changes every seed's program.
const (
	percent = 100

	maxFileTypes    = 2
	maxStructs      = 3
	maxStructFields = 3
	pctBuiltinFile  = 25

	// Base types, in order: int, float, string, bool, file, path, else a
	// struct.
	intBelow    = 20
	floatBelow  = 32
	stringBelow = 47
	boolBelow   = 54
	fileBelow   = 70
	pathBelow   = 75

	// Type shapes, in order: scalar, T[], T[][], then (where maps are
	// allowed) map<T>, map<T[]>, else map<T>[].
	scalarBelow    = 55
	arrayBelow     = 75
	array2DBelow   = 82
	mapBelow       = 94
	mapOfArrsBelow = 97

	pctNullLiteral = 8
	pctTrue        = 40
	pctEmpty       = 35
	maxCollLen     = 3

	maxEntryInputs = 3
	minEntryCalls  = 3
	maxEntryCalls  = 7
	maxSubCalls    = 3
	maxDeepCalls   = 2
	maxDepth       = 2

	// Call kinds, in order: a new sub pipeline, a declared callable again, a
	// wildcard-bound stage, else a new stage, which a sub maps more often:
	// its collection inputs are what its call sites vary.
	newSubBelow       = 18
	existingBelow     = 50
	wildcardBelow     = 55
	pctMapNewSub      = 45
	pctMapExisting    = 35
	pctMapNewStage    = 35
	pctMapNewSubStage = 60
	pctPickSub        = 70

	pctGate         = 22
	pctGateFromCall = 60
	pctGateSafe     = 75
	pctSafeBool     = 60

	minEntryReturns   = 2
	maxEntryReturns   = 5
	maxSubReturns     = 3
	pctLiteralReturn  = 10
	pctFanInReturn    = 10
	maxNewStageInputs = 3
	maxStageOuts      = 3
	pctBoolOut        = 30
	pctSplitStage     = 30
	maxChunkIns       = 2
	maxChunkOuts      = 2
	pctFailStage      = 7
	pctFailError      = 30
	minFailMod        = 2
	maxFailMod        = 4

	pctZip              = 30
	pctSplitFromScope   = 70
	pctSplitFromInput   = 50
	pctSplitMap         = 40
	collectionTypeTries = 10
	pctZipSameGroup     = 70
	pctZipShapedLiteral = 60

	// A new stage input's source, in order: a source in scope, a fan-in, a
	// fresh pipeline input, else a literal.
	scopeValueBelow = 55
	fanInValueBelow = 65
	inputValueBelow = 72
	pctIntAsFloat   = 10
	pctMapFanIn     = 35
	minFanIn        = 2
	maxFanIn        = 3
	pctFanInSource  = 60

	// An existing callee's input, in order: a source of its type, a fan-in,
	// a fresh pipeline input, else a literal.
	scopeBindBelow     = 55
	fanInBindBelow     = 63
	inputBindBelow     = 73
	scopeBindSubBelow  = 40
	fanInBindSubBelow  = 48
	inputBindSubBelow  = 70
	pctSubBindFromCall = 70

	maxSubSeeds         = 2
	pctCollectionSeed   = 50
	pctWildcardFromCall = 50
	pctWildcardParam    = 60

	// The relative odds a source is a pipeline input, a call output, a whole
	// call or a projection, and how many fields deep projections go.
	weightSelf         = 20
	weightOut          = 40
	weightWhole        = 15
	weightProjection   = 25
	maxProjectionDepth = 2
)
