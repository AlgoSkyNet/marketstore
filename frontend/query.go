package frontend

import (
	"net/http"
	"sync/atomic"
	"time"

	"fmt"

	"strings"

	"github.com/alpacahq/marketstore/SQLParser"
	"github.com/alpacahq/marketstore/executor"
	"github.com/alpacahq/marketstore/planner"
	"github.com/alpacahq/marketstore/utils"
	"github.com/alpacahq/marketstore/utils/io"
	"github.com/alpacahq/marketstore/utils/log"
)

type QueryRequest struct {
	IsSQLStatement     bool             `msgpack:"issqlstatement"` // If this is a SQL request, Only SQLStatement is relevant
	SQLStatement       string           `msgpack:"sqlstatement"`
	Destination        io.TimeBucketKey `msgpack:"destination"`
	TimeStart          int64            `msgpack:"timestart"` // Unix Epoch based time limits
	TimeEnd            int64            `msgpack:"timeend"`   // Unix Epoch based time limits
	LimitRecordCount   int              `msgpack:"limitrecordcount"`
	TimeOrderAscending bool             `msgpack:"timeorderascending"` // If Nrecords is non-zero, order the records ascending in time
	Functions          []string         `msgpack:"functions"`
}

type MultiQueryRequest struct {
	/*
		A multi-request allows for different Timeframes and record formats for each request
	*/
	Requests []QueryRequest `msgpack:"requests"`
}

type QueryResponse struct {
	Result *io.NumpyMultiDataset `msgpack:"result"`
}

type MultiQueryResponse struct {
	Responses []QueryResponse `msgpack:"responses"`
	Version   string          `msgpack:"version"`  // Server Version
	Timezone  string          `msgpack:"timezone"` // Server timezone
}

func (s *DataService) Query(r *http.Request, reqs *MultiQueryRequest, response *MultiQueryResponse) (err error) {
	response.Version = utils.Version
	response.Timezone = utils.InstanceConfig.Timezone.String()
	for _, req := range reqs.Requests {
		switch req.IsSQLStatement {
		case true:
			ast, err := SQLParser.NewAstBuilder(req.SQLStatement)
			if err != nil {
				return err
			}
			es, err := SQLParser.NewExecutableStatement(ast.Mtree)
			if err != nil {
				return err
			}
			cs, err := es.Materialize()
			if err != nil {
				return err
			}
			nds, err := io.NewNumpyDataset(cs)
			if err != nil {
				return err
			}
			tbk, err := io.NewTimeBucketKeyFromString(req.SQLStatement + ":SQL")
			if err != nil {
				return err
			}
			nmds, err := io.NewNumpyMultiDataset(nds, *tbk)
			if err != nil {
				return err
			}
			response.Responses = append(response.Responses,
				QueryResponse{
					nmds,
				})

		case false:
			/*
				Assumption: Within each TimeBucketKey, we have one or more of each category, with the exception of
				the AttributeGroup (aka Record Format) and Timeframe

				Within each TimeBucketKey in the request, we allow for a comma separated list of items, e.g.:
					destination1.items := "TSLA,AAPL,CG/1Min/OHLCV"

				Constraints:
				- If there is more than one record format in a single destination, we return an error
				- If there is more than one Timeframe in a single destination, we return an error
			*/
			dest, err := io.NewTimeBucketKeyFromString(req.Destination.Key)
			if err != nil {
				return err
			}
			/*
				All destinations in a request must share the same record format (AttributeGroup) and Timeframe
			*/
			RecordFormat := dest.GetItemInCategory("AttributeGroup")
			Timeframe := dest.GetItemInCategory("Timeframe")
			Symbols := dest.GetMultiItemInCategory("Symbol")

			if len(Timeframe) == 0 || len(RecordFormat) == 0 || len(Symbols) == 0 {
				return fmt.Errorf("destinations must have a Symbol, Timeframe and AttributeGroup, have: %s",
					dest.String())
			}

			systemTz := utils.InstanceConfig.Timezone
			start := time.Unix(req.TimeStart, 0).In(systemTz)
			stop := time.Unix(req.TimeEnd, 0).In(systemTz)

			csm, tpm, err := executeQuery(
				dest,
				start, stop,
				req.LimitRecordCount, req.TimeOrderAscending,
			)
			if err != nil {
				return err
			}

			/*
				Execute function pipeline, if requested
			*/
			if len(req.Functions) != 0 {
				for tbkStr, cs := range csm {
					csOut, err := runAggFunctions(req.Functions, cs)
					if err != nil {
						return err
					}
					csm[tbkStr] = csOut
				}
			}

			/*
				Separate each TimeBucket from the result and compose a NumpyMultiDataset
			*/
			var nmds *io.NumpyMultiDataset
			for tbk, cs := range csm {
				nds, err := io.NewNumpyDataset(cs)
				if err != nil {
					return err
				}
				if nmds == nil {
					nmds, err = io.NewNumpyMultiDataset(nds, tbk)
					if err != nil {
						return err
					}
				} else {
					nmds.Append(cs, tbk)
				}
			}

			/*
				Append the NumpyMultiDataset to the MultiResponse
			*/
			tpmStr := make(map[string]int64)
			for key, val := range tpm {
				tpmStr[key.String()] = val
			}
			response.Responses = append(response.Responses,
				QueryResponse{
					nmds,
				})

		}
	}
	return nil
}

type RangeLimitArgs struct {
	Destination io.TimeBucketKey `msgpack:"destination"`
}

type RangeLimitReply struct {
	Start int64 //Unix Epoch Timestamp
	End   int64 //Unix Epoch Timestamp
}

func (s *DataService) RangeLimit(r *http.Request, args *RangeLimitArgs, response *RangeLimitReply) (err error) {
	if atomic.LoadUint32(&Queryable) == 0 {
		return queryableError
	}
	if args == nil {
		return argsNilError
	}
	tbk, err := io.NewTimeBucketKeyFromString(args.Destination.Key)
	if err != nil {
		return err
	}

	query := planner.NewQuery(executor.ThisInstance.CatalogDir)
	query.AddTargetKey(tbk)

	// Let's get the first record
	query.SetRowLimit(io.FIRST, 1)
	parseResult, err := query.Parse()
	if err != nil {
		log.Log(log.ERROR, "Parsing query: %s\n", err)
		return err
	}
	scanner, err := executor.NewReader(parseResult)
	if err != nil {
		log.Log(log.ERROR, "Unable to create new reader", err)
		return err
	}
	startResultMap, _, err := scanner.Read()

	query.SetRowLimit(io.LAST, 1)
	parseResult, err = query.Parse()
	if err != nil {
		log.Log(log.ERROR, "Parsing query: %s\n", err)
		return err
	}
	scanner, err = executor.NewReader(parseResult)
	endResultMap, _, err := scanner.Read()

	for key, cs := range startResultMap {
		response.Start = cs.GetEpoch()[0]
		response.End = endResultMap[key].GetEpoch()[0]
	}
	return err
}

type ListSymbolsReply struct {
	Results []string
}

type ListSymbolsArgs struct{}

func (s *DataService) ListSymbols(r *http.Request, args *ListSymbolsArgs, response *ListSymbolsReply) (err error) {
	if atomic.LoadUint32(&Queryable) == 0 {
		return queryableError
	}
	for symbol := range executor.ThisInstance.CatalogDir.GatherCategoriesAndItems()["Symbol"] {
		response.Results = append(response.Results, symbol)
	}
	return err
}

/*
Utility functions
*/

func executeQuery(tbk *io.TimeBucketKey, start, end time.Time, LimitRecordCount int,
	TimeOrderAscending bool) (io.ColumnSeriesMap, map[io.TimeBucketKey]int64, error) {

	query := planner.NewQuery(executor.ThisInstance.CatalogDir)

	/*
		Alter timeframe inside key to ensure it matches a queryable TF
	*/
	tf := tbk.GetItemInCategory("Timeframe")
	cd := utils.CandleDurationFromString(tf)
	queryableTimeframe := cd.QueryableTimeframe()
	tbk.SetItemInCategory("Timeframe", queryableTimeframe)
	query.AddTargetKey(tbk)

	if LimitRecordCount != 0 {
		direction := io.LAST
		if TimeOrderAscending {
			direction = io.FIRST
		}
		query.SetRowLimit(
			direction,
			cd.QueryableNrecords(
				queryableTimeframe,
				LimitRecordCount,
			),
		)
	}

	query.SetRange(start, end)
	parseResult, err := query.Parse()
	if err != nil {
		// No results from query
		if err.Error() == "No files returned from query parse" {
			log.Log(log.INFO, "No results returned from query: Target: %v, start, end: %v,%v LimitRecordCount: %v",
				tbk.String(), start, end, LimitRecordCount)
		} else {
			log.Log(log.ERROR, "Parsing query: %s\n", err)
		}
		return nil, nil, err
	}
	scanner, err := executor.NewReader(parseResult)
	if err != nil {
		log.Log(log.ERROR, "Unable to create scanner: %s\n", err)
		return nil, nil, err
	}
	csm, tPrevMap, err := scanner.Read()
	if err != nil {
		log.Log(log.ERROR, "Error returned from query scanner: %s\n", err)
		return nil, nil, err
	}
	return csm, tPrevMap, err
}

func runAggFunctions(callChain []string, csInput *io.ColumnSeries) (cs *io.ColumnSeries, err error) {
	cs = nil
	for _, call := range callChain {
		if cs != nil {
			csInput = cs
		}
		aggName, literalList, parameterList, err := parseFunctionCall(call)
		if err != nil {
			return nil, err
		}

		agg := SQLParser.AggRegistry[strings.ToLower(aggName)]
		if agg == nil {
			return nil, fmt.Errorf("No function in the UDA Registry named \"%s\"", aggName)
		}
		aggfunc, argMap := agg.New()

		err = argMap.PrepareArguments(parameterList)
		if err != nil {
			return nil, fmt.Errorf("Argument mapping error for %s: %s", aggName, err.Error())
		}

		/*
			Initialize the Aggregate
				An agg may have init parameters, which are used only to initialize it
				These are single value literals (like '1Min')
		*/
		requiredInitDSV := aggfunc.GetInitArgs()
		requiredInitNames := io.GetNamesFromDSV(requiredInitDSV)

		if len(requiredInitNames) > len(literalList) {
			return nil, fmt.Errorf(
				"Not enough init arguments for %s, need %d have %d",
				aggName,
				len(requiredInitNames),
				len(literalList),
			)
		}
		aggfunc.Init(literalList)

		/*
			Execute the aggregate function
		*/
		if err = aggfunc.Accum(csInput); err != nil {
			return nil, err
		}
		cs = aggfunc.Output()
		if cs == nil {
			return nil, fmt.Errorf(
				"No result from aggregate %s",
				aggName)
		}
	}
	return cs, nil
}

func parseFunctionCall(call string) (funcName string, literalList, parameterList []string, err error) {
	call = strings.Trim(call, " ")
	left := strings.Index(call, "(")
	right := strings.LastIndex(call, ")")
	if left == -1 || right == -1 {
		return "", nil, nil, fmt.Errorf("unable to parse function call %s", call)
	}
	funcName = strings.Trim(call[:left], " ")
	call = call[left+1 : right]
	/*
		First parse for literals and re-form a string without them for the last stage of parsing
	*/
	var newCall string
	for {
		left = strings.Index(call, "'")
		if left == -1 {
			newCall = newCall + call
			break
		} else if left != 0 {
			newCall = newCall + call[:left]
		}
		call = call[left+1:]
		right = strings.Index(call, "'")
		if right == -1 {
			return "", nil, nil, fmt.Errorf("unclosed literal %s", call)
		}
		literalList = append(literalList, call[:right])
		call = call[right+1:]
	}
	pList := strings.Split(newCall, ",")
	for _, val := range pList {
		trimmed := strings.Trim(val, " ")
		if len(trimmed) != 0 {
			parameterList = append(parameterList, trimmed)
		}
	}
	return funcName, literalList, parameterList, nil
}
