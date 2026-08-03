// sgctl 是拾光集的命令行工具，目前提供批量导入子命令。
//
// 单二进制、纯 Go 编译，可放在任何能访问服务的机器上运行：
//
//	sgctl import ~/Photos --server https://your-host --token $SG_ADMIN_TOKEN
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"shiguang/internal/importer"
)

const usage = `拾光集命令行工具

用法：
  sgctl import <目录> [选项]      批量导入目录下的照片
  sgctl version                   显示版本

import 选项：
  --server <url>      服务地址（默认 $SG_SERVER，或 http://localhost:8080）
  --token <token>     管理口令（默认 $SG_ADMIN_TOKEN）
  --group <mode>      分组方式，默认 auto：
                        auto   子目录内的按目录名归组，根目录散图按拍摄日期
                        date   一律按 EXIF 拍摄日期
                        folder 一律按第一层目录名，散图归入「未分类」
                        single 全部放进同一个节点
  --title <标题>      --group=single 时的节点标题（默认「批量导入」）
  --concurrency <n>   并发上传数（默认 4）
  --max-mb <n>        单张大小上限 MB（默认 30，需与服务端一致）
  --dry-run           只扫描并打印将要创建的节点，不上传
  --emit-manifest     在各节点目录生成 shiguang.txt 模板供编辑（不覆盖已有）
  --timeout <dur>     单次请求超时（默认 5m）

示例：
  sgctl import ~/Photos --server https://photos.example.com --token $SG_ADMIN_TOKEN
  sgctl import ./老照片 --group folder --dry-run
  sgctl import ./老照片 --emit-manifest      # 先生成清单，改完再导入

指定日期与文案：在照片目录里放一个 shiguang.txt（可用 --emit-manifest 生成）：

  date = 2019-10-06
  title = 小妹的婚礼
  description = 全家去了三亚，海边办的
  DSC1000.jpg = 接亲那天早上
  DSC1001.jpg = 海边仪式

没有这个文件时，一切按自动推导走，行为与不用清单完全一致。

重跑安全：照片按内容去重，中断后重跑会自动跳过已导入的，只补没传完的部分。
`

// version 由构建时 -ldflags 注入。
var version = "dev"

func main() {
	if len(os.Args) < 2 {
		fmt.Fprint(os.Stderr, usage)
		os.Exit(2)
	}
	switch os.Args[1] {
	case "import":
		if err := runImport(os.Args[2:]); err != nil {
			fmt.Fprintf(os.Stderr, "\n错误：%v\n", err)
			os.Exit(1)
		}
	case "version", "-v", "--version":
		fmt.Printf("sgctl %s\n", version)
	case "help", "-h", "--help":
		fmt.Print(usage)
	default:
		fmt.Fprintf(os.Stderr, "未知命令 %q\n\n%s", os.Args[1], usage)
		os.Exit(2)
	}
}

func runImport(args []string) error {
	fs := flag.NewFlagSet("import", flag.ContinueOnError)
	fs.Usage = func() { fmt.Fprint(os.Stderr, usage) }
	var (
		server      = fs.String("server", envOr("SG_SERVER", "http://localhost:8080"), "")
		token       = fs.String("token", os.Getenv("SG_ADMIN_TOKEN"), "")
		group       = fs.String("group", "auto", "")
		title       = fs.String("title", "批量导入", "")
		concurrency = fs.Int("concurrency", 4, "")
		maxMB       = fs.Int64("max-mb", 30, "")
		dryRun      = fs.Bool("dry-run", false, "")
		emit        = fs.Bool("emit-manifest", false, "")
		timeout     = fs.Duration("timeout", 5*time.Minute, "")
	)
	// flag 包在首个位置参数处停止解析，而文档用法是 `import <目录> [选项]`，
	// 所以先把目录摘出来，再解析剩下的选项。
	root, rest := splitPositional(args)
	if err := fs.Parse(rest); err != nil {
		return err
	}
	if root == "" {
		if fs.NArg() > 0 {
			root = fs.Arg(0)
		} else {
			fs.Usage()
			return errors.New("请指定要导入的目录")
		}
	}

	mode, err := importer.ParseGroupMode(*group)
	if err != nil {
		return err
	}
	if *token == "" && !*dryRun && !*emit {
		return errors.New("缺少管理口令：用 --token 指定，或设置环境变量 SG_ADMIN_TOKEN")
	}

	// Ctrl-C 优雅中断：已传完的照片保留，重跑会接着补
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	opt := importer.Options{
		Root:        root,
		GroupMode:   mode,
		SingleTitle: *title,
		Concurrency: *concurrency,
		MaxBytes:    *maxMB << 20,
		DryRun:      *dryRun,
	}

	var client *importer.Client
	if !*dryRun && !*emit {
		client, err = importer.NewClient(*server, *token, *timeout)
		if err != nil {
			return err
		}
		mode, err := client.Ping(ctx)
		if err != nil {
			var apiErr *importer.APIError
			if errors.As(err, &apiErr) && apiErr.Status == 401 {
				return errors.New("口令无效，请检查 --token / SG_ADMIN_TOKEN")
			}
			return fmt.Errorf("连接服务失败（%s）：%w", *server, err)
		}
		opt.UploadMode = mode
		fmt.Printf("已连接 %s（存储模式：%s）\n", *server, mode)
	}

	// dry-run 与 emit 都只需要前置的扫描/分组/套清单，走与真正导入同一条路径
	if *dryRun || *emit {
		groups, photos, warnings, err := importer.ScanAndPlan(root, mode, *title, opt.MaxBytes)
		if err != nil {
			return err
		}
		if *emit {
			return emitManifests(root, groups, mode, warnings)
		}
		printPlan(root, groups, photos, warnings)
		fmt.Println("\n这是 --dry-run，没有上传任何东西。去掉该参数即可真正导入。")
		return nil
	}

	// 进度显示：终端里单行原地刷新；重定向到文件/CI 时改为定期换行输出，
	// 否则满屏 \r 拼在一起没法看。
	tty := isTerminal(os.Stdout)
	var (
		lastLen  int
		progMu   sync.Mutex
		lastTick time.Time
	)
	opt.Progress = func(s importer.Stat) {
		progMu.Lock()
		defer progMu.Unlock()
		if tty {
			line := fmt.Sprintf("\r上传中 %d/%d  已传 %d  已存在 %d  失败 %d  %s",
				s.Done, s.Total, s.Uploaded, s.Skipped, s.Failed, truncate(s.Current, 32))
			if pad := lastLen - len([]rune(line)); pad > 0 {
				line += strings.Repeat(" ", pad)
			}
			lastLen = len([]rune(line))
			fmt.Print(line)
			return
		}
		// 非终端：每 2 秒或最后一张时打一行
		if s.Done < s.Total && time.Since(lastTick) < 2*time.Second {
			return
		}
		lastTick = time.Now()
		fmt.Printf("上传中 %d/%d  已传 %d  已存在 %d  失败 %d\n",
			s.Done, s.Total, s.Uploaded, s.Skipped, s.Failed)
	}

	res, err := importer.Run(ctx, client, opt)
	if tty {
		fmt.Println()
	}
	if err != nil {
		if errors.Is(err, context.Canceled) {
			fmt.Println("已中断。重跑同一条命令会跳过已导入的照片，接着补完。")
			return nil
		}
		return err
	}

	fmt.Printf("\n完成，用时 %s\n", humanDuration(res.Elapsed))
	fmt.Printf("  节点：新建 %d 个，复用 %d 个\n", res.NodesCreated, res.NodesReused)
	fmt.Printf("  照片：上传 %d 张，已存在跳过 %d 张，失败 %d 张\n",
		res.Uploaded, res.Skipped, res.Failed)
	if len(res.Errors) > 0 {
		fmt.Printf("\n以下 %d 项未处理：\n", len(res.Errors))
		for i, e := range res.Errors {
			if i >= 15 {
				fmt.Printf("  …… 另有 %d 项\n", len(res.Errors)-15)
				break
			}
			fmt.Printf("  %s\n", e)
		}
	}
	if res.Uploaded > 0 {
		fmt.Println("\n照片正在后台显影，稍后刷新前台时间轴即可看到。")
	}
	return nil
}

// printPlan 打印将要创建的节点，并标注日期可信度与各类警告。
func printPlan(root string, groups []*importer.Group, photos []*importer.Photo, warnings []string) {
	fmt.Printf("扫描 %s：%d 张可导入照片，将归入 %d 个节点\n\n", root, len(photos), len(groups))

	var untrusted []*importer.Group
	captioned := 0
	for _, p := range photos {
		if p.Caption != "" {
			captioned++
		}
	}
	for _, g := range groups {
		mark := "  "
		switch {
		case g.DateFromManifest:
			mark = "✎ " // 日期由清单指定
		case !g.DateFromEXIF:
			mark = "⚠ " // 日期来自文件修改时间，多半不是真实拍摄日
			untrusted = append(untrusted, g)
		}
		fmt.Printf("  %s%s  %-28s %3d 张（%d 张带 EXIF 拍摄时间）\n",
			mark, g.Date, truncate(g.Title, 28), len(g.Photos), g.EXIFCount())
		if g.Description != "" {
			fmt.Printf("      描述：%s\n", truncate(g.Description, 60))
		}
	}
	if captioned > 0 {
		fmt.Printf("\n✎ 清单文件生效：%d 张照片使用了自定义图注\n", captioned)
	}

	if len(untrusted) > 0 {
		fmt.Printf("\n⚠ 有 %d 个节点的日期来自「文件修改时间」而非 EXIF 拍摄时间：\n", len(untrusted))
		for i, g := range untrusted {
			if i >= 8 {
				fmt.Printf("    …… 另有 %d 个\n", len(untrusted)-8)
				break
			}
			fmt.Printf("    %s  %s\n", g.Date, g.Title)
		}
		fmt.Println("  扫描件、被聊天软件转发过的照片通常没有 EXIF，这些日期多半不是真实拍摄日。")
		fmt.Printf("  可用 --emit-manifest 生成 %s 后填写正确日期，或导入后在后台修改。\n",
			importer.ManifestName)
	}

	if len(warnings) > 0 {
		fmt.Printf("\n以下 %d 项需要注意：\n", len(warnings))
		for i, s := range warnings {
			if i >= 10 {
				fmt.Printf("  …… 另有 %d 项\n", len(warnings)-10)
				break
			}
			fmt.Printf("  %s\n", s)
		}
	}
}

// emitManifests 生成清单模板并汇报结果。
func emitManifests(root string, groups []*importer.Group, mode importer.GroupMode,
	warnings []string) error {

	res, err := importer.EmitManifests(root, groups, mode)
	if err != nil {
		return err
	}
	if len(res.Written) > 0 {
		fmt.Printf("已生成 %d 份清单模板：\n", len(res.Written))
		for _, p := range res.Written {
			fmt.Printf("  %s\n", p)
		}
	}
	if len(res.Skipped) > 0 {
		fmt.Printf("\n已存在、未覆盖 %d 份（你的改动不会被冲掉）：\n", len(res.Skipped))
		for i, p := range res.Skipped {
			if i >= 10 {
				fmt.Printf("  …… 另有 %d 份\n", len(res.Skipped)-10)
				break
			}
			fmt.Printf("  %s\n", p)
		}
	}
	if len(res.NoDir) > 0 {
		fmt.Printf("\n以下 %d 个节点没有唯一对应的目录，无处安放清单：\n", len(res.NoDir))
		for i, g := range res.NoDir {
			if i >= 8 {
				fmt.Printf("  …… 另有 %d 个\n", len(res.NoDir)-8)
				break
			}
			fmt.Printf("  %s  %s（%d 张）\n", g.Date, g.Title, len(g.Photos))
		}
		fmt.Println("  这些是散在根目录、按拍摄日期归组的照片。把它们放进子文件夹，")
		fmt.Println("  或改用 --group folder / --group single，就能为其生成清单。")
	}
	for _, w := range warnings {
		fmt.Printf("  %s\n", w)
	}
	if len(res.Written) == 0 && len(res.Skipped) == 0 {
		fmt.Println("没有生成任何清单。")
		return nil
	}
	fmt.Printf("\n用文本编辑器改完这些 %s，再运行一次 sgctl import 即可按清单导入。\n",
		importer.ManifestName)
	return nil
}

// splitPositional 取出第一个非选项参数作为目录，其余原样返回。
// 需要跳过「--flag value」中的 value，否则会把选项值误当成目录。
func splitPositional(args []string) (positional string, rest []string) {
	// 需要跟值的选项（bool 选项写成 --dry-run 不带值）
	takesValue := map[string]bool{
		"server": true, "token": true, "group": true, "title": true,
		"concurrency": true, "max-mb": true, "timeout": true,
	}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if strings.HasPrefix(a, "-") {
			rest = append(rest, a)
			name := strings.TrimLeft(a, "-")
			// --flag=value 形式自带值，不吃下一个参数
			if !strings.Contains(a, "=") && takesValue[name] && i+1 < len(args) {
				i++
				rest = append(rest, args[i])
			}
			continue
		}
		if positional == "" {
			positional = a
			continue
		}
		rest = append(rest, a)
	}
	return positional, rest
}

// isTerminal 判断输出是否为终端（决定进度条形态），仅用标准库。
func isTerminal(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// humanDuration 让亚秒级耗时不显示成 "0s"。
func humanDuration(d time.Duration) string {
	if d < time.Second {
		return d.Round(time.Millisecond).String()
	}
	return d.Round(100 * time.Millisecond).String()
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}

// truncate 按显示宽度截断（按 rune 计，避免截断多字节字符）。
func truncate(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	if n <= 1 {
		return string(r[:n])
	}
	return string(r[:n-1]) + "…"
}
