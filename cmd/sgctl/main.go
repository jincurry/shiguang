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
  --timeout <dur>     单次请求超时（默认 5m）

示例：
  sgctl import ~/Photos --server https://photos.example.com --token $SG_ADMIN_TOKEN
  sgctl import ./老照片 --group folder --dry-run

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
	if *token == "" && !*dryRun {
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
	if !*dryRun {
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

	// dry-run 先把计划打出来
	if *dryRun {
		photos, skipped, err := importer.Scan(root, opt.MaxBytes)
		if err != nil {
			return err
		}
		groups := importer.Plan(photos, mode, *title)
		fmt.Printf("扫描 %s：%d 张可导入照片，将归入 %d 个节点\n\n", root, len(photos), len(groups))

		var noEXIF []*importer.Group
		for _, g := range groups {
			mark := "  "
			if !g.DateFromEXIF {
				mark = "⚠ " // 日期来自文件修改时间，多半不是真实拍摄日
				noEXIF = append(noEXIF, g)
			}
			fmt.Printf("  %s%s  %-28s %3d 张（%d 张带 EXIF 拍摄时间）\n",
				mark, g.Date, truncate(g.Title, 28), len(g.Photos), g.EXIFCount())
		}

		if len(noEXIF) > 0 {
			fmt.Printf("\n⚠ 有 %d 个节点的日期来自「文件修改时间」而非 EXIF 拍摄时间：\n", len(noEXIF))
			for i, g := range noEXIF {
				if i >= 8 {
					fmt.Printf("    …… 另有 %d 个\n", len(noEXIF)-8)
					break
				}
				fmt.Printf("    %s  %s\n", g.Date, g.Title)
			}
			fmt.Println("  扫描件、被聊天软件转发过的照片通常没有 EXIF，这些日期多半不是真实拍摄日。")
			fmt.Println("  导入后请在管理后台逐个核对修改；或先按事件整理成文件夹，导入后改节点日期更省事。")
		}

		if len(skipped) > 0 {
			fmt.Printf("\n跳过 %d 个文件：\n", len(skipped))
			for i, s := range skipped {
				if i >= 10 {
					fmt.Printf("  …… 另有 %d 个\n", len(skipped)-10)
					break
				}
				fmt.Printf("  %s\n", s)
			}
		}
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
