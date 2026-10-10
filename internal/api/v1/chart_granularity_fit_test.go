package v1_test

// chartGranularityFitCase is one (timeframe, requested grain) request
// and the grain the response must both READ and REPORT.
type chartGranularityFitCase struct {
	timeframe string
	requested string
	served    string
	why       string
}
