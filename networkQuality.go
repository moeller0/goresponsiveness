/*
 * This file is part of Go Responsiveness.
 *
 * Go Responsiveness is free software: you can redistribute it and/or modify it under
 * the terms of the GNU General Public License as published by the Free Software Foundation,
 * either version 2 of the License, or (at your option) any later version.
 * Go Responsiveness is distributed in the hope that it will be useful, but WITHOUT ANY
 * WARRANTY; without even the implied warranty of MERCHANTABILITY or FITNESS FOR A
 * PARTICULAR PURPOSE. See the GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License along
 * with Go Responsiveness. If not, see <https://www.gnu.org/licenses/>.
 */

package main

import (
	"context"
	"flag"
	"fmt"
	"os"
	"runtime/pprof"
	"time"

	"github.com/network-quality/goresponsiveness/ccw"
	"github.com/network-quality/goresponsiveness/config"
	"github.com/network-quality/goresponsiveness/constants"
	"github.com/network-quality/goresponsiveness/datalogger"
	"github.com/network-quality/goresponsiveness/debug"
	"github.com/network-quality/goresponsiveness/extendedstats"
	"github.com/network-quality/goresponsiveness/lgc"
	"github.com/network-quality/goresponsiveness/rpm"
	"github.com/network-quality/goresponsiveness/timeoutat"
	"github.com/network-quality/goresponsiveness/utilities"
)

var (
	// Variables to hold CLI arguments.
	configHost = flag.String(
		"config",
		constants.DefaultConfigHost,
		"name/IP of responsiveness configuration server.",
	)
	configPort = flag.Int(
		"port",
		constants.DefaultPortNumber,
		"port number on which to access responsiveness configuration server.",
	)
	configPath = flag.String(
		"path",
		"config",
		"path on the server to the configuration endpoint.",
	)
	debugCliFlag = flag.Bool(
		"debug",
		constants.DefaultDebug,
		"Enable debugging.",
	)
	sattimeout = flag.Int(
		"sattimeout",
		constants.DefaultTestTime,
		"Maximum time to spend measuring saturation.",
	)
	rpmtimeout = flag.Int(
		"rpmtimeout",
		constants.RPMCalculationTime,
		"Maximum time to spend calculating RPM.",
	)
	sslKeyFileName = flag.String(
		"ssl-key-file",
		"",
		"Store the per-session SSL key files in this file.",
	)
	profile = flag.String(
		"profile",
		"",
		"Enable client runtime profiling and specify storage location. Disabled by default.",
	)
	calculateExtendedStats = flag.Bool(
		"extended-stats",
		false,
		"Enable the collection and display of extended statistics -- may not be available on certain platforms.",
	)
	dataLoggerBaseFileName = flag.String(
		"logger-filename",
		"",
		"Store granular information about tests results in files with this basename. Time and information type will be appended (before the first .) to create separate log files. Disabled by default.",
	)
)

func main() {
	flag.Parse()

	timeoutDuration := time.Second * time.Duration(*sattimeout)
	timeoutAbsoluteTime := time.Now().Add(timeoutDuration)
	configHostPort := fmt.Sprintf("%s:%d", *configHost, *configPort)

	// This is the overall operating context of the program. All other
	// contexts descend from this one. Canceling this one cancels all
	// the others.
	operatingCtx, cancelOperatingCtx := context.WithCancel(context.Background())

	//
	lgDataCollectionCtx, cancelLGDataCollectionCtx := context.WithCancel(operatingCtx)

	// This context is used to control the load-generating network activity (i.e., all
	// the connections that are open to do load generation).
	lgNetworkActivityCtx, cancelLgNetworkActivityCtx := context.WithCancel(operatingCtx)

	// This context is used to control the activity of the foreign prober.
	foreignProbertCtx, foreignProberCtxCancel := context.WithCancel(operatingCtx)
	config := &config.Config{}
	var debugLevel debug.DebugLevel = debug.Error

	if *debugCliFlag {
		debugLevel = debug.Debug
	}

	if *calculateExtendedStats && !extendedstats.ExtendedStatsAvailable() {
		*calculateExtendedStats = false
		fmt.Printf(
			"Warning: Calculation of extended statistics was requested but they are not supported on this platform.\n",
		)
	}

	if err := config.Get(configHostPort, *configPath); err != nil {
		fmt.Fprintf(os.Stderr, "%s\n", err)
		return
	}
	if err := config.IsValid(); err != nil {
		fmt.Fprintf(
			os.Stderr,
			"Error: Invalid configuration returned from %s: %v\n",
			config.Source,
			err,
		)
		return
	}
	if debug.IsDebug(debugLevel) {
		fmt.Printf("Configuration: %s\n", config)
	}

	timeoutChannel := timeoutat.TimeoutAt(
		operatingCtx,
		timeoutAbsoluteTime,
		debugLevel,
	)
	if debug.IsDebug(debugLevel) {
		fmt.Printf("Test will end earlier than %v\n", timeoutAbsoluteTime)
	}

	// print the banner
	dt := time.Now().UTC()
	fmt.Printf(
		"%s UTC Go Responsiveness to %s...\n",
		dt.Format("01-02-2006 15:04:05"),
		configHostPort,
	)

	if len(*profile) != 0 {
		f, err := os.Create(*profile)
		if err != nil {
			fmt.Fprintf(
				os.Stderr,
				"Error: Profiling requested with storage in %s but that file could not be opened: %v\n",
				*profile,
				err,
			)
			return
		}
		pprof.StartCPUProfile(f)
		defer pprof.StopCPUProfile()
	}

	var sslKeyFileConcurrentWriter *ccw.ConcurrentWriter = nil
	if *sslKeyFileName != "" {
		if sslKeyFileHandle, err := os.OpenFile(*sslKeyFileName, os.O_RDWR|os.O_CREATE, os.FileMode(0600)); err != nil {
			fmt.Printf("Could not open the keyfile for writing: %v!\n", err)
			sslKeyFileConcurrentWriter = nil
		} else {
			if err = utilities.SeekForAppend(sslKeyFileHandle); err != nil {
				fmt.Printf("Could not seek to the end of the key file: %v!\n", err)
				sslKeyFileConcurrentWriter = nil
			} else {
				if debug.IsDebug(debugLevel) {
					fmt.Printf("Doing SSL key logging through file %v\n", *sslKeyFileName)
				}
				sslKeyFileConcurrentWriter = ccw.NewConcurrentFileWriter(sslKeyFileHandle)
				defer sslKeyFileHandle.Close()
			}
		}
	}

	var selfDataLogger datalogger.DataLogger[rpm.ProbeDataPoint] = nil
	var foreignDataLogger datalogger.DataLogger[rpm.ProbeDataPoint] = nil
	var downloadThroughputDataLogger datalogger.DataLogger[rpm.ThroughputDataPoint] = nil
	var uploadThroughputDataLogger datalogger.DataLogger[rpm.ThroughputDataPoint] = nil
	// User wants to log data from each probe!
	if *dataLoggerBaseFileName != "" {
		var err error = nil
		unique := time.Now().UTC().Format("01-02-2006-15-04-05")

		dataLoggerSelfFilename := utilities.FilenameAppend(*dataLoggerBaseFileName, "-self-"+unique)
		dataLoggerForeignFilename := utilities.FilenameAppend(
			*dataLoggerBaseFileName,
			"-foreign-"+unique,
		)
		dataLoggerDownloadThroughputFilename := utilities.FilenameAppend(
			*dataLoggerBaseFileName,
			"-throughput-download"+unique,
		)
		dataLoggerUploadThroughputFilename := utilities.FilenameAppend(
			*dataLoggerBaseFileName,
			"-throughput-upload"+unique,
		)

		selfDataLogger, err = datalogger.CreateCSVDataLogger[rpm.ProbeDataPoint](
			dataLoggerSelfFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing self probe results (%s). Disabling functionality.\n",
				dataLoggerSelfFilename,
			)
			selfDataLogger = nil
		}

		foreignDataLogger, err = datalogger.CreateCSVDataLogger[rpm.ProbeDataPoint](
			dataLoggerForeignFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing foreign probe results (%s). Disabling functionality.\n",
				dataLoggerForeignFilename,
			)
			foreignDataLogger = nil
		}

		downloadThroughputDataLogger, err = datalogger.CreateCSVDataLogger[rpm.ThroughputDataPoint](
			dataLoggerDownloadThroughputFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing download throughput results (%s). Disabling functionality.\n",
				dataLoggerDownloadThroughputFilename,
			)
			downloadThroughputDataLogger = nil
		}

		uploadThroughputDataLogger, err = datalogger.CreateCSVDataLogger[rpm.ThroughputDataPoint](
			dataLoggerUploadThroughputFilename,
		)
		if err != nil {
			fmt.Printf(
				"Warning: Could not create the file for storing upload throughput results (%s). Disabling functionality.\n",
				dataLoggerUploadThroughputFilename,
			)
			uploadThroughputDataLogger = nil
		}
	}

	/*
	 * Create (and then, ironically, name) two anonymous functions that, when invoked,
	 * will create load-generating connections for upload/download
	 */
	generate_lgd := func() lgc.LoadGeneratingConnection {
		return &lgc.LoadGeneratingConnectionDownload{
			Path:      config.Urls.LargeUrl,
			KeyLogger: sslKeyFileConcurrentWriter,
		}
	}
	generate_lgu := func() lgc.LoadGeneratingConnection {
		return &lgc.LoadGeneratingConnectionUpload{
			Path:      config.Urls.UploadUrl,
			KeyLogger: sslKeyFileConcurrentWriter,
		}
	}

	generateSelfProbeConfiguration := func() rpm.ProbeConfiguration {
		return rpm.ProbeConfiguration{
			URL:        config.Urls.SmallUrl,
			DataLogger: selfDataLogger,
			Interval:   100 * time.Millisecond,
		}
	}

	generateForeignProbeConfiguration := func() rpm.ProbeConfiguration {
		return rpm.ProbeConfiguration{
			URL:        config.Urls.SmallUrl,
			DataLogger: foreignDataLogger,
			Interval:   100 * time.Millisecond,
		}
	}

	var downloadDebugging *debug.DebugWithPrefix = debug.NewDebugWithPrefix(debugLevel, "download")
	var uploadDebugging *debug.DebugWithPrefix = debug.NewDebugWithPrefix(debugLevel, "upload")
	var foreignDebugging *debug.DebugWithPrefix = debug.NewDebugWithPrefix(debugLevel, "foreign probe")

	// TODO: Separate contexts for load generation and data collection. If we do that, if either of the two
	// data collection go routines stops well before the other, they will continue to send probes and we can
	// generate additional information!

	downloadSaturationComplete, downloadDataCollectionChannel := rpm.LGCollectData(
		lgDataCollectionCtx,
		lgNetworkActivityCtx,
		operatingCtx,
		generate_lgd,
		generateSelfProbeConfiguration,
		downloadThroughputDataLogger,
		downloadDebugging,
	)
	uploadSaturationComplete, uploadDataCollectionChannel := rpm.LGCollectData(
		lgDataCollectionCtx,
		lgNetworkActivityCtx,
		operatingCtx,
		generate_lgu,
		generateSelfProbeConfiguration,
		uploadThroughputDataLogger,
		uploadDebugging,
	)

	foreignProbeDataPointsChannel := rpm.ForeignProber(
		foreignProbertCtx,
		generateForeignProbeConfiguration,
		sslKeyFileConcurrentWriter,
		foreignDebugging,
	)

	dataCollectionTimeout := false
	uploadDataGenerationComplete := false
	downloadDataGenerationComplete := false
	downloadDataCollectionResult := rpm.SelfDataCollectionResult{}
	uploadDataCollectionResult := rpm.SelfDataCollectionResult{}

	for !(uploadDataGenerationComplete && downloadDataGenerationComplete) {
		select {
		case fullyComplete := <-downloadSaturationComplete:
			{
				downloadDataGenerationComplete = true
				if *debugCliFlag {
					fmt.Printf(
						"################# download load-generating data generation is %s complete!\n",
						utilities.Conditional(fullyComplete, "", "(provisionally)"))
				}
			}
		case fullyComplete := <-uploadSaturationComplete:
			{
				uploadDataGenerationComplete = true
				if *debugCliFlag {
					fmt.Printf(
						"################# upload load-generating data generation is %s complete!\n",
						utilities.Conditional(fullyComplete, "", "(provisionally)"))
				}
			}
		case <-timeoutChannel:
			{
				if dataCollectionTimeout {
					// We already timedout on data collection. This signal means that
					// we are timedout on getting the provisional data collection. We
					// will exit!
					fmt.Fprint(
						os.Stderr,
						"Error: Load-Generating data collection could not be completed in time and no provisional data could be gathered. Test failed.\n",
					)
					cancelOperatingCtx()
					if *debugCliFlag {
						time.Sleep(constants.CooldownPeriod)
					}
					return // Ends program
				}
				dataCollectionTimeout = true

				// We timed out attempting to collect data about the link. So, we will
				// shut down the generators
				cancelLGDataCollectionCtx()
				// and then we will give ourselves some additional time in order
				// to see if we can get some provisional data.
				timeoutAbsoluteTime = time.Now().
					Add(time.Second * time.Duration(*rpmtimeout))
				timeoutChannel = timeoutat.TimeoutAt(
					operatingCtx,
					timeoutAbsoluteTime,
					debugLevel,
				)
				if *debugCliFlag {
					fmt.Printf(
						"################# timeout collecting load-generating data!\n",
					)
				}
			}
		}
	}

	if *debugCliFlag {
		fmt.Printf("Stopping all the load generating data generators.\n")
	}
	// Just cancel the data collection -- do *not* yet stop the actual load-generating
	// network activity.
	cancelLGDataCollectionCtx()

	// Shutdown the foreign-connection prober!
	if *debugCliFlag {
		fmt.Printf("Stopping all foreign probers.\n")
	}
	foreignProberCtxCancel()

	// Now that we stopped generation, let's give ourselves some time to collect
	// all the data from our data generators.
	timeoutAbsoluteTime = time.Now().
		Add(time.Second * time.Duration(*rpmtimeout))
	timeoutChannel = timeoutat.TimeoutAt(
		operatingCtx,
		timeoutAbsoluteTime,
		debugLevel,
	)

	// Now that we have generated the data, let's collect it.
	downloadDataCollectionComplete := false
	uploadDataCollectionComplete := false
	for !(downloadDataCollectionComplete && uploadDataCollectionComplete) {
		select {
		case downloadDataCollectionResult = <-downloadDataCollectionChannel:
			{
				downloadDataCollectionComplete = true
				if *debugCliFlag {
					fmt.Printf(
						"################# download load-generating data collection is complete (%fMBps, %d flows)!\n",
						utilities.ToMBps(downloadDataCollectionResult.RateBps),
						len(downloadDataCollectionResult.LGCs),
					)
				}
			}
		case uploadDataCollectionResult = <-uploadDataCollectionChannel:
			{
				uploadDataCollectionComplete = true
				if *debugCliFlag {
					fmt.Printf(
						"################# upload load-generating data collection is complete (%fMBps, %d flows)!\n",
						utilities.ToMBps(uploadDataCollectionResult.RateBps),
						len(uploadDataCollectionResult.LGCs),
					)
				}
			}
		case <-timeoutChannel:
			{
				// This is just bad news -- we generated data but could not collect it. Let's just fail.

				fmt.Fprint(
					os.Stderr,
					"Error: Load-Generating data collection could not be completed in time and no provisional data could be gathered. Test failed.\n",
				)
				return // Ends program
			}
		}
	}

	// In the new version we are no longer going to wait to send probes until after
	// saturation. When we get here we are now only going to compute the results
	// and/or extended statistics!

	extendedStats := extendedstats.AggregateExtendedStats{}

	if *calculateExtendedStats {
		if extendedstats.ExtendedStatsAvailable() {
			for i := 0; i < len(downloadDataCollectionResult.LGCs); i++ {
				// Assume that extended statistics are available -- the check was done explicitly at
				// program startup if the calculateExtendedStats flag was set by the user on the command line.
				if err := extendedStats.IncorporateConnectionStats(downloadDataCollectionResult.LGCs[i].Stats().ConnInfo.Conn); err != nil {
					fmt.Fprintf(
						os.Stderr,
						"Warning: Could not add extended stats for the connection: %v\n",
						err,
					)
				}
			}
		} else {
			// TODO: Should we just log here?
			panic("Extended stats are not available but the user requested their calculation.")
		}
	}

	// And only now, when we are done getting the extended stats from the connections, can
	// we actually shut down the load-generating network activity!
	cancelLgNetworkActivityCtx()

	fmt.Printf(
		"Download: %7.3f Mbps (%7.3f MBps), using %d parallel connections.\n",
		utilities.ToMbps(downloadDataCollectionResult.RateBps),
		utilities.ToMBps(downloadDataCollectionResult.RateBps),
		len(downloadDataCollectionResult.LGCs),
	)
	fmt.Printf(
		"Upload:   %7.3f Mbps (%7.3f MBps), using %d parallel connections.\n",
		utilities.ToMbps(uploadDataCollectionResult.RateBps),
		utilities.ToMBps(uploadDataCollectionResult.RateBps),
		len(uploadDataCollectionResult.LGCs),
	)

	foreignProbeDataPoints := utilities.ChannelToSlice(foreignProbeDataPointsChannel)
	totalForeignRoundTrips := len(foreignProbeDataPoints)
	// The specification indicates that we want to calculate the foreign probes as such:
	// 1/3*tcp_foreign + 1/3*tls_foreign + 1/3*http_foreign
	// where tcp_foreign, tls_foreign, http_foreign are the P90 RTTs for the connection
	// of the tcp, tls and http connections, respectively. However, we cannot break out
	// the individual RTTs so we assume that they are roughly equal. Call that _foreign:
	// 1/3*_foreign + 1/3*_foreign + 1/3*_foreign =
	// 1/3*(3*_foreign) =
	// _foreign
	// So, there's no need to divide by the number of RTTs defined in the ProbeDataPoints
	// in the individual results.
	foreignProbeRoundTripTimes := utilities.Fmap(
		foreignProbeDataPoints,
		func(dp rpm.ProbeDataPoint) float64 { return dp.Duration.Seconds() },
	)
	foreignProbeRoundTripTimeP90 := utilities.CalculatePercentile(foreignProbeRoundTripTimes, 90)

	downloadRoundTripTimes := utilities.Fmap(
		downloadDataCollectionResult.ProbeDataPoints,
		func(dcr rpm.ProbeDataPoint) float64 { return dcr.Duration.Seconds() },
	)
	uploadRoundTripTimes := utilities.Fmap(
		uploadDataCollectionResult.ProbeDataPoints,
		func(dcr rpm.ProbeDataPoint) float64 { return dcr.Duration.Seconds() },
	)
	selfProbeRoundTripTimes := append(downloadRoundTripTimes, uploadRoundTripTimes...)
	totalSelfRoundTrips := len(selfProbeRoundTripTimes)
	selfProbeRoundTripTimeP90 := utilities.CalculatePercentile(selfProbeRoundTripTimes, 90)

	rpm := 60.0 / (float64(selfProbeRoundTripTimeP90+foreignProbeRoundTripTimeP90) / 2.0)

	if *debugCliFlag {
		fmt.Printf(
			"Total Load-Generating Round Trips: %d, Total New-Connection Round Trips: %d, P90 LG RTT: %f, P90 NC RTT: %f\n",
			totalSelfRoundTrips,
			totalForeignRoundTrips,
			selfProbeRoundTripTimeP90,
			foreignProbeRoundTripTimeP90,
		)
	}

	fmt.Printf("RPM: %5.0f\n", rpm)

	if *calculateExtendedStats {
		fmt.Println(extendedStats.Repr())
	}

	if !utilities.IsInterfaceNil(selfDataLogger) {
		selfDataLogger.Export()
		if *debugCliFlag {
			fmt.Printf("Closing the self data logger.\n")
		}
		selfDataLogger.Close()
	}

	if !utilities.IsInterfaceNil(foreignDataLogger) {
		foreignDataLogger.Export()
		if *debugCliFlag {
			fmt.Printf("Closing the foreign data logger.\n")
		}
		foreignDataLogger.Close()
	}

	if !utilities.IsInterfaceNil(downloadThroughputDataLogger) {
		downloadThroughputDataLogger.Export()
		if *debugCliFlag {
			fmt.Printf("Closing the download throughput data logger.\n")
		}
		downloadThroughputDataLogger.Close()
	}

	if !utilities.IsInterfaceNil(uploadThroughputDataLogger) {
		uploadThroughputDataLogger.Export()
		if *debugCliFlag {
			fmt.Printf("Closing the upload throughput data logger.\n")
		}
		uploadThroughputDataLogger.Close()
	}

	cancelOperatingCtx()
	if *debugCliFlag {
		fmt.Printf("In debugging mode, we will cool down.\n")
		time.Sleep(constants.CooldownPeriod)
		fmt.Printf("Done cooling down.\n")
	}
}
