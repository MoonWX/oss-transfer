package main

import (
	"bufio"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/bits-and-blooms/bloom/v3"
)

func cmdDiffObjects(args []string, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	if len(args) < 3 {
		return cliErrorf("用法: diff-objects <源目录> <有序目标清单> <输出目录>；详见 help tools")
	}

	srcDir := args[0]
	destManifest := args[1]
	outputDir := args[2]
	mode := "sort-merge"
	falsePositiveRate := 0.01

	totalStartTime := time.Now()

	for i := 3; i < len(args); i++ {
		if args[i] == "--mode" && i+1 < len(args) {
			mode = args[i+1]
			i++
		} else if args[i] == "--false-positive" && i+1 < len(args) {
			rate, err := strconv.ParseFloat(args[i+1], 64)
			if err != nil || rate <= 0 || rate >= 1 {
				return cliErrorf("--false-positive 必须大于 0 且小于 1: %s", args[i+1])
			}
			falsePositiveRate = rate
			i++
		}
	}

	if mode != "bloom" && mode != "sort-merge" {
		return cliErrorf("不支持的模式: %s，请使用 bloom 或 sort-merge", mode)
	}

	fmt.Fprintf(out, cliText(out, "提取增量对象...\n"))
	fmt.Fprintf(out, cliText(out, "  源清单目录: %s\n"), srcDir)
	fmt.Fprintf(out, cliText(out, "  目标清单: %s\n"), destManifest)
	fmt.Fprintf(out, cliText(out, "  输出目录: %s\n"), outputDir)
	fmt.Fprintf(out, cliText(out, "  模式: %s\n"), mode)

	srcDirInfo, err := os.Stat(srcDir)
	if err != nil {
		return cliErrorf("源目录不存在: %v", err)
	}
	if !srcDirInfo.IsDir() {
		return cliErrorf("源清单路径不是目录: %s", srcDir)
	}

	err = os.MkdirAll(outputDir, 0755)
	if err != nil {
		return cliErrorf("创建输出目录失败: %v", err)
	}

	destInfo, err := os.Stat(destManifest)
	if err != nil {
		return cliErrorf("目标清单文件不存在: %v", err)
	}

	if mode == "bloom" {
		destFileSize := destInfo.Size()
		lineCount, err := countLines(destManifest)
		if err != nil {
			return cliErrorf("统计目标清单行数失败: %v", err)
		}

		avgKeySize := int(destFileSize) / lineCount
		if avgKeySize < 50 {
			avgKeySize = 50
		}

		fmt.Fprintf(out, cliText(out, "  目标对象数: %d\n"), lineCount)

		bitsPerKey := -math.Log(falsePositiveRate) / (math.Ln2 * math.Ln2)
		memSize := int(float64(lineCount) * bitsPerKey / 8)
		fmt.Fprintf(out, cliText(out, "\n预计资源使用:"))
		fmt.Fprintf(out, cliText(out, "  内存: ~%s\n"), formatSize(int64(memSize)))
		fmt.Fprintf(out, cliText(out, "  磁盘: ~0\n"))
		fmt.Fprintf(out, cliText(out, "  准确率: %.4f%% \n"), (1-falsePositiveRate)*100)

		return cmdDiffObjectsBloom(srcDir, destManifest, outputDir, lineCount, falsePositiveRate, totalStartTime, out)
	} else {
		fmt.Fprintf(out, cliText(out, "\n预计资源使用:"))
		fmt.Fprintf(out, cliText(out, "  内存: ~10*单个源清单大小\n"))
		fmt.Fprintf(out, cliText(out, "  磁盘: ~0\n"))
		fmt.Fprintf(out, cliText(out, "  准确率: 100%%\n"))

		return cmdDiffObjectsSortMerge(srcDir, destManifest, outputDir, "", totalStartTime, out)
	}
}

func countLines(filename string) (int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	count := 0
	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	for scanner.Scan() {
		if scanner.Text() != "" {
			count++
		}
	}

	return count, scanner.Err()
}

func cmdDiffObjectsBloom(srcDir, destManifest, outputDir string, expectedCount int, falsePositiveRate float64, totalStartTime time.Time, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	fmt.Fprintln(out, cliText(out, "\n[1/3] 构建目标bucket对象布隆过滤器..."))
	buildStartTime := time.Now()
	filter, totalCount, err := buildBloomFilterWithCount(destManifest, expectedCount, falsePositiveRate)
	if err != nil {
		return cliErrorf("构建布隆过滤器失败: %v", err)
	}
	buildElapsed := time.Since(buildStartTime)
	fmt.Fprintf(out, cliText(out, "  已加载 %d 个目标对象到布隆过滤器，误判率: %.4f%%，耗时: %s\n"), totalCount, falsePositiveRate*100, buildElapsed)

	processStartTime := time.Now()
	err = processSourceManifests(srcDir, outputDir, func(key string) bool {
		return filter.Test([]byte(key))
	}, out)
	processElapsed := time.Since(processStartTime)
	totalElapsed := time.Since(totalStartTime)

	fmt.Fprintf(out, cliText(out, "\n  构建过滤器耗时: %s\n"), buildElapsed)
	fmt.Fprintf(out, cliText(out, "  筛选增量对象耗时: %s\n"), processElapsed)
	fmt.Fprintf(out, cliText(out, "  总耗时: %s\n"), totalElapsed)

	return err
}

func buildBloomFilterWithCount(filename string, expectedCount int, falsePositiveRate float64) (*bloom.BloomFilter, int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return nil, 0, err
	}
	defer file.Close()

	filter := bloom.NewWithEstimates(uint(expectedCount), falsePositiveRate)
	count := 0

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	for scanner.Scan() {
		line := scanner.Text()
		if line != "" {
			key := strings.SplitN(line, " ", 2)[0]
			filter.Add([]byte(key))
			count++
		}
	}

	if err := scanner.Err(); err != nil {
		return nil, 0, err
	}

	return filter, count, nil
}

func cmdDiffObjectsSortMerge(srcDir, destManifest, outputDir, customTmpDir string, totalStartTime time.Time, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	fmt.Fprintln(out, cliText(out, "\n[1/4] 遍历源bucket对象清单目录..."))
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return cliErrorf("读取源目录失败: %v", err)
	}

	var srcFiles []string
	var totalSrcSize int64
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		if strings.HasSuffix(fileName, ".csv") || strings.HasSuffix(fileName, ".CSV") {
			info, _ := entry.Info()
			totalSrcSize += info.Size()
			srcFiles = append(srcFiles, srcDir+"/"+fileName)
		}
	}

	fmt.Fprintf(out, cliText(out, "  找到 %d 个CSV文件，总大小: %s\n"), len(srcFiles), formatSize(totalSrcSize))

	destFile, err := os.Open(destManifest)
	if err != nil {
		return cliErrorf("打开目标清单失败: %v", err)
	}
	destStat, _ := destFile.Stat()
	destFileSize := destStat.Size()
	fmt.Fprintf(out, cliText(out, "  目标清单大小: %s\n"), formatSize(destFileSize))
	destFile.Close()

	sortedDir := "./sorted-src/"
	os.RemoveAll(sortedDir)
	err = os.MkdirAll(sortedDir, 0755)
	if err != nil {
		return cliErrorf("创建排序目录失败: %v", err)
	}

	fmt.Fprintln(out, cliText(out, "\n[2/4] 并发排序源清单并写入磁盘..."))

	type SortResult struct {
		Index      int
		SortedFile string
		KeyCount   int
		Err        error
	}

	sem := make(chan struct{}, 300)
	sortResults := make(chan SortResult, len(srcFiles))

	var sortWg sync.WaitGroup
	for idx := range srcFiles {
		sortWg.Add(1)
		go func(index int) {
			defer sortWg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			srcFile := srcFiles[index]
			sortedFile, keyCount, err := sortSourceManifestToDisk(srcFile, sortedDir)
			sortResults <- SortResult{
				Index:      index,
				SortedFile: sortedFile,
				KeyCount:   keyCount,
				Err:        err,
			}
		}(idx)
	}

	go func() {
		sortWg.Wait()
		close(sortResults)
	}()

	sortedFiles := make(map[int]string)
	totalKeys := 0
	sortedCount := 0
	for result := range sortResults {
		sortedCount++
		if result.Err != nil {
			return cliErrorf("排序源清单失败 %s: %v", srcFiles[result.Index], result.Err)
		}
		sortedFiles[result.Index] = result.SortedFile
		totalKeys += result.KeyCount
		if sortedCount%10 == 0 || sortedCount == len(srcFiles) {
			elapsed := time.Since(totalStartTime).Seconds()
			fmt.Fprintf(out, cliText(out, "  已排序 %d/%d 个文件，累计对象: %d，耗时: %.1fs\n"), sortedCount, len(srcFiles), totalKeys, elapsed)
		}
	}

	sortElapsed := time.Since(totalStartTime)
	fmt.Fprintf(out, cliText(out, "  排序完成，总对象数: %d，耗时: %s\n"), totalKeys, sortElapsed)

	mergeStartTime := time.Now()
	fmt.Fprintln(out, cliText(out, "\n[3/4] 并发归并筛选增量对象..."))
	type ProcessResult struct {
		FileName string
		Count    int
		Err      error
	}

	results := make(chan ProcessResult, len(srcFiles))

	var wg sync.WaitGroup
	mergeSem := make(chan struct{}, 300)
	for idx := range srcFiles {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			mergeSem <- struct{}{}
			defer func() { <-mergeSem }()

			srcFile := srcFiles[index]
			sortedFile := sortedFiles[index]
			fileName := filepath.Base(srcFile)
			outputFile := outputDir + "/" + fileName + ".incr"

			count, err := diffSortedFileWithDest(sortedFile, destManifest, outputFile)
			results <- ProcessResult{
				FileName: fileName,
				Count:    count,
				Err:      err,
			}
		}(idx)
	}

	go func() {
		wg.Wait()
		close(results)
	}()

	totalIncrCount := 0
	fileCount := 0
	errorCount := 0
	lastPrintTime := time.Now()

	for result := range results {
		fileCount++
		if result.Err != nil {
			errorCount++
			fmt.Fprintf(out, cliText(out, "  [错误] %s: %v\n"), result.FileName, result.Err)
		} else {
			totalIncrCount += result.Count
			now := time.Now()
			if now.Sub(lastPrintTime) >= 3*time.Second || fileCount%10 == 0 || fileCount == len(srcFiles) {
				elapsed := time.Since(mergeStartTime).Seconds()
				percent := float64(fileCount) / float64(len(srcFiles)) * 100
				fmt.Fprintf(out, cliText(out, "  进度: %d/%d (%.1f%%)，累计增量对象: %d，耗时: %.1fs\n"),
					fileCount, len(srcFiles), percent, totalIncrCount, elapsed)
				lastPrintTime = now
			}
		}
	}

	mergeElapsed := time.Since(mergeStartTime)

	fmt.Fprintln(out, cliText(out, "\n[4/4] 清理临时文件..."))
	os.RemoveAll(sortedDir)

	fmt.Fprintln(out, cliText(out, "\n处理完成!"))
	fmt.Fprintf(out, cliText(out, "  处理文件数: %d\n"), fileCount)
	fmt.Fprintf(out, cliText(out, "  成功文件数: %d\n"), fileCount-errorCount)
	fmt.Fprintf(out, cliText(out, "  总增量对象: %d\n"), totalIncrCount)
	if errorCount > 0 {
		fmt.Fprintf(out, cliText(out, "  失败文件数: %d\n"), errorCount)
	}
	fmt.Fprintf(out, cliText(out, "  排序耗时: %s\n"), sortElapsed)
	fmt.Fprintf(out, cliText(out, "  归并耗时: %s\n"), mergeElapsed)
	fmt.Fprintf(out, cliText(out, "  总耗时: %s\n"), time.Since(totalStartTime))

	return nil
}

type SrcObject struct {
	Key     string
	Updated time.Time
}

func sortSourceManifestToDisk(srcFile, sortedDir string) (string, int, error) {
	inFile, err := os.Open(srcFile)
	if err != nil {
		return "", 0, err
	}
	defer inFile.Close()

	var objects []SrcObject
	scanner := bufio.NewScanner(inFile)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	isFirstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if isFirstLine && strings.HasPrefix(line, "project,bucket,name,") {
			isFirstLine = false
			continue
		}
		isFirstLine = false

		parts := strings.SplitN(line, ",", 5)
		if len(parts) >= 4 {
			key := parts[2]
			updatedStr := parts[3]
			updated, err := time.Parse(time.RFC3339Nano, updatedStr)
			if err != nil {
				updated = time.Time{}
			}
			objects = append(objects, SrcObject{Key: key, Updated: updated})
		}
	}

	if err := scanner.Err(); err != nil {
		return "", 0, err
	}

	sort.Slice(objects, func(i, j int) bool {
		return objects[i].Key < objects[j].Key
	})

	fileName := filepath.Base(srcFile)
	sortedFile := sortedDir + fileName + ".sorted"
	outFile, err := os.Create(sortedFile)
	if err != nil {
		return "", 0, err
	}
	defer outFile.Close()

	writer := bufio.NewWriterSize(outFile, 64*1024*1024)
	for _, obj := range objects {
		writer.WriteString(obj.Key)
		if !obj.Updated.IsZero() {
			writer.WriteString("\t")
			writer.WriteString(obj.Updated.Format(time.RFC3339Nano))
		}
		writer.WriteString("\n")
	}
	writer.Flush()

	return sortedFile, len(objects), nil
}

func diffSortedFileWithDest(sortedFile, destManifest, outputFile string) (int, error) {
	srcFile, err := os.Open(sortedFile)
	if err != nil {
		return 0, err
	}
	defer srcFile.Close()

	destFile, err := os.Open(destManifest)
	if err != nil {
		return 0, err
	}
	defer destFile.Close()

	outFile, err := os.Create(outputFile)
	if err != nil {
		return 0, err
	}
	defer outFile.Close()

	srcScanner := bufio.NewScanner(srcFile)
	srcBuf := make([]byte, 0, 64*1024*1024)
	srcScanner.Buffer(srcBuf, 1024*1024*1024)

	destScanner := bufio.NewScanner(destFile)
	destBuf := make([]byte, 0, 64*1024*1024)
	destScanner.Buffer(destBuf, 1024*1024*1024)

	count := 0
	var srcKey string
	var destKey string

	srcHasMore := srcScanner.Scan()
	if srcHasMore {
		srcLine := srcScanner.Text()
		srcParts := strings.SplitN(srcLine, "\t", 2)
		srcKey = srcParts[0]
	}

	destHasMore := destScanner.Scan()
	if destHasMore {
		destLine := destScanner.Text()
		destParts := strings.SplitN(destLine, " ", 4)
		destKey = destParts[0]
	}

	for srcHasMore {
		for destHasMore && destKey < srcKey {
			destHasMore = destScanner.Scan()
			if destHasMore {
				destLine := destScanner.Text()
				destParts := strings.SplitN(destLine, " ", 4)
				destKey = destParts[0]
			}
		}

		if !destHasMore || destKey > srcKey {
			outFile.WriteString(srcKey + "\n")
			count++
		}

		srcHasMore = srcScanner.Scan()
		if srcHasMore {
			srcLine := srcScanner.Text()
			srcParts := strings.SplitN(srcLine, "\t", 2)
			srcKey = srcParts[0]
		}
	}

	if err := srcScanner.Err(); err != nil {
		return 0, err
	}
	if err := destScanner.Err(); err != nil {
		return 0, err
	}

	return count, nil
}

func processSourceManifests(srcDir, outputDir string, existsFunc func(string) bool, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	fmt.Fprintln(out, cliText(out, "\n[2/3] 遍历源bucket对象清单目录..."))
	entries, err := os.ReadDir(srcDir)
	if err != nil {
		return cliErrorf("读取源目录失败: %v", err)
	}

	var files []string
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		fileName := entry.Name()
		if !strings.HasSuffix(fileName, ".csv") && !strings.HasSuffix(fileName, ".CSV") {
			continue
		}
		files = append(files, fileName)
	}

	fmt.Fprintf(out, cliText(out, "  找到 %d 个CSV文件，开始并发处理...\n"), len(files))

	type ProcessResult struct {
		FileName string
		Count    int
		Err      error
	}

	numWorkers := 50
	if len(files) < numWorkers {
		numWorkers = len(files)
	}

	jobs := make(chan string, len(files))
	results := make(chan ProcessResult, len(files))

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for fileName := range jobs {
				srcFile := srcDir + "/" + fileName
				outputFile := outputDir + "/" + fileName + ".incr"
				count, err := processSrcManifestWithCheck(srcFile, outputFile, existsFunc)
				results <- ProcessResult{
					FileName: fileName,
					Count:    count,
					Err:      err,
				}
			}
		}()
	}

	for _, fileName := range files {
		jobs <- fileName
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	totalIncrCount := 0
	fileCount := 0
	errorCount := 0

	for result := range results {
		fileCount++
		if result.Err != nil {
			errorCount++
			fmt.Fprintf(out, cliText(out, "  [错误] %s: %v\n"), result.FileName, result.Err)
		} else {
			totalIncrCount += result.Count
			if fileCount%10 == 0 {
				fmt.Fprintf(out, cliText(out, "  已处理 %d/%d 个文件，累计增量对象: %d\n"), fileCount, len(files), totalIncrCount)
			}
		}
	}

	fmt.Fprintln(out, cliText(out, "\n[3/3] 处理完成!"))
	fmt.Fprintf(out, cliText(out, "  处理文件数: %d\n"), fileCount)
	fmt.Fprintf(out, cliText(out, "  成功文件数: %d\n"), fileCount-errorCount)
	fmt.Fprintf(out, cliText(out, "  总增量对象: %d\n"), totalIncrCount)
	if errorCount > 0 {
		fmt.Fprintf(out, cliText(out, "  失败文件数: %d\n"), errorCount)
	}

	return nil
}

func processSrcManifestWithCheck(srcFile, outputFile string, existsFunc func(string) bool) (int, error) {
	inFile, err := os.Open(srcFile)
	if err != nil {
		return 0, err
	}
	defer inFile.Close()

	outFile, err := os.Create(outputFile)
	if err != nil {
		return 0, err
	}
	defer outFile.Close()

	count := 0
	scanner := bufio.NewScanner(inFile)
	buf := make([]byte, 0, 64*1024*1024)
	scanner.Buffer(buf, 1024*1024*1024)

	isFirstLine := true
	for scanner.Scan() {
		line := scanner.Text()
		if line == "" {
			continue
		}

		if isFirstLine && strings.HasPrefix(line, "project,bucket,name,") {
			isFirstLine = false
			continue
		}
		isFirstLine = false

		parts := strings.SplitN(line, ",", 4)
		if len(parts) >= 3 {
			key := parts[2]
			if !existsFunc(key) {
				outFile.WriteString(key + "\n")
				count++
			}
		}
	}

	return count, scanner.Err()
}

func cmdCountLines(args []string, outputs ...io.Writer) error {
	out := offlineCLIOutput(outputs)
	if len(args) < 1 {
		return cliErrorf("用法: count-lines <目录>")
	}

	dir := args[0]

	dirInfo, err := os.Stat(dir)
	if err != nil {
		return cliErrorf("目录不存在: %v", err)
	}
	if !dirInfo.IsDir() {
		return cliErrorf("路径不是目录: %s", dir)
	}

	fmt.Fprintf(out, cliText(out, "统计目录: %s\n"), dir)
	fmt.Fprintln(out, cliText(out, "扫描文件..."))

	entries, err := os.ReadDir(dir)
	if err != nil {
		return cliErrorf("读取目录失败: %v", err)
	}

	type FileResult struct {
		Lines int
		Err   error
	}

	files := make([]string, 0)
	for _, entry := range entries {
		if !entry.IsDir() {
			files = append(files, dir+"/"+entry.Name())
		}
	}

	fmt.Fprintf(out, cliText(out, "找到 %d 个文件，开始并发统计...\n"), len(files))

	numWorkers := 10
	if len(files) < numWorkers {
		numWorkers = len(files)
	}

	jobs := make(chan string, len(files))
	results := make(chan FileResult, len(files))

	var wg sync.WaitGroup
	for w := 0; w < numWorkers; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for file := range jobs {
				lines, err := countFileLines(file)
				results <- FileResult{Lines: lines, Err: err}
			}
		}()
	}

	for _, file := range files {
		jobs <- file
	}
	close(jobs)

	go func() {
		wg.Wait()
		close(results)
	}()

	totalLines := 0
	fileCount := 0
	errorCount := 0

	for result := range results {
		if result.Err != nil {
			errorCount++
		} else {
			totalLines += result.Lines
			fileCount++
			if fileCount%100 == 0 {
				fmt.Fprintf(out, cliText(out, "已处理 %d 个文件，累计 %d 行...\n"), fileCount, totalLines)
			}
		}
	}

	fmt.Fprintln(out, "================================")
	fmt.Fprintf(out, cliText(out, "文件总数: %d\n"), fileCount)
	fmt.Fprintf(out, cliText(out, "总行数: %d\n"), totalLines)
	if errorCount > 0 {
		fmt.Fprintf(out, cliText(out, "失败文件: %d\n"), errorCount)
	}

	return nil
}

func countFileLines(filename string) (int, error) {
	file, err := os.Open(filename)
	if err != nil {
		return 0, err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	buf := make([]byte, 0, 64*1024)
	scanner.Buffer(buf, 1024*1024)

	lines := 0
	for scanner.Scan() {
		lines++
	}

	if err := scanner.Err(); err != nil {
		return 0, err
	}

	return lines, nil
}
