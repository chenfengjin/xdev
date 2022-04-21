package cmd

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/docker/docker/api/types"
	"github.com/docker/docker/api/types/container"
	"github.com/docker/docker/api/types/mount"
	"github.com/docker/docker/api/types/network"
	"github.com/docker/docker/api/types/volume"
	"github.com/docker/docker/client"
	"github.com/spf13/cobra"
	"github.com/xuperchain/xdev/mkfile"
)

type lintCommand struct {
	cxxFlags            []string
	ldflags             []string
	builder             *mkfile.Builder
	entryPkg            *mkfile.Package
	UsingPrecompiledSDK bool
	NoEntry             bool
	xdevRoot            string
	buildMod            string
	ccImage             string

	genCompileCommand bool
	makeFileOnly      bool
	output            string
	compiler          string
	makeFlags         string
	submodules        []string
}

func newLintCommand() *cobra.Command {
	c := &lintCommand{
		ldflags:  defaultLDFlags,
		cxxFlags: defaultCxxFlags,
	}

	cmd := &cobra.Command{
		Use:   "lint",
		Short: "build command builds a project",
		RunE: func(cmd *cobra.Command, args []string) error {
			if c.UsingPrecompiledSDK {
				c.ldflags = append(c.ldflags, fmt.Sprintf("-L%s/lib", mkfile.DefaultXROOT), "-lxchain", "-lprotobuf-lite")
				c.ldflags = append(c.ldflags, fmt.Sprintf("--js-library %s/src/xchain/exports.js", mkfile.DefaultXROOT))
				c.cxxFlags = append(c.cxxFlags, fmt.Sprintf("-I%s/src", mkfile.DefaultXROOT))
			} else {
				xroot := os.Getenv("XDEV_ROOT")
				c.xdevRoot = xroot
				c.ldflags = append(c.ldflags, fmt.Sprintf("--js-library %s/src/xchain/exports.js", xroot))
			}
			if c.NoEntry {
				c.ldflags = append(c.ldflags, "--no-entry")
			}
			// CCImage 优先级：环境变量 > 默认值
			// 1. 如果是debug 模式，则采用debugImage
			// 2. 如果有环境变量，则以环境变量为准

			c.ccImage = ccImageRelease
			if c.buildMod == buildModeDebug {
				c.ccImage = ccImageDebug
			}

			if image := os.Getenv("XDEV_CC_IMAGE"); image != "" {
				c.ccImage = image
			}

			if c.buildMod == buildModeDebug {
				c.cxxFlags = append(c.cxxFlags, debugBuildFlags...)
				c.ldflags = append(c.ldflags, debugLinkFlags...)
			} else if c.buildMod == buildModeRelease {
				c.cxxFlags = append(c.cxxFlags, releaseBuildFlags...)
				c.ldflags = append(c.ldflags, releaseLinkFlags...)
			}
			return c.build(args)
		},
	}
	cmd.Flags().BoolVarP(&c.makeFileOnly, "makefile", "m", false, "generate makefile and exit")
	cmd.Flags().BoolVarP(&c.genCompileCommand, "compile_command", "p", false, "generate compile_commands.json for IDE")
	cmd.Flags().StringVarP(&c.output, "output", "o", "", "output file name")
	cmd.Flags().StringVarP(&c.compiler, "compiler", "", "docker", "compiler env docker|host")
	cmd.Flags().StringVarP(&c.makeFlags, "mkflags", "", "", "extra flags passing to make command")
	cmd.Flags().StringSliceVarP(&c.submodules, "submodule", "s", nil, "build submodules")
	cmd.Flags().BoolVarP(&c.UsingPrecompiledSDK, "using-precompiled-sdk", "", true, "using precompiled sdk")
	cmd.Flags().BoolVarP(&c.NoEntry, "no-entry", "", true, "do not output any entry point")
	cmd.Flags().StringVarP(&c.buildMod, "build-mode", "", buildModeRelease, "build mode, may be debug or release")
	// cmd.Flags().StringVarP(&c.ccImage, "cc-image", "", ccImageRelease, "")
	return cmd
}

func (c *lintCommand) parsePackage(root, xcache string) error {
	absroot, err := filepath.Abs(root)
	if err != nil {
		return err
	}

	addons, err := c.addonModules(absroot)
	if err != nil {
		return err
	}
	if c.submodules != nil {
		addons = append(addons, mkfile.DependencyDesc{
			Name:    "self",
			Modules: c.submodules,
		})
	}

	loader := mkfile.NewLoader().WithXROOT(c.xdevRoot)
	pkg, err := loader.Load(absroot, addons)
	if err != nil {
		return err
	}

	output := c.output
	// 如果没有指定输出，且为main package，则用package目录名+wasm后缀作为输出名字
	if output == "" && pkg.Name == mkfile.MainPackage {
		output = filepath.Base(absroot) + ".wasm"
	}

	if output != "" {
		c.output, err = filepath.Abs(output)
		if err != nil {
			return err
		}
	}

	b := mkfile.NewBuilder().
		WithCxxFlags(c.cxxFlags).
		WithLDFlags(c.ldflags).
		WithCacheDir(xcache).
		WithOutput(c.output)

	err = b.Parse(pkg)
	if err != nil {
		return err
	}
	c.builder = b
	c.entryPkg = pkg
	return nil
}

func (c *lintCommand) xdevCacheDir() (string, error) {
	xcache := os.Getenv("XDEV_CACHE")
	if xcache != "" {
		return filepath.Abs(xcache)
	}
	homedir, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(homedir, ".xdev-cache"), nil
}

func (c *lintCommand) build(args []string) error {
	var err error
	if c.output != "" && !filepath.IsAbs(c.output) {
		c.output, err = filepath.Abs(c.output)
		if err != nil {
			return err
		}
	}

	if len(args) == 0 {
		root, err := findPackageRoot()
		if err != nil {
			return err
		}
		return c.lintPackage(root)
	}

	return c.lintFiles(args)
}

func (c *lintCommand) addonModules(pkgpath string) ([]mkfile.DependencyDesc, error) {
	desc, err := mkfile.ParsePackageDesc(pkgpath)
	if err != nil {
		return nil, err
	}
	if desc.Package.Name != mkfile.MainPackage {
		return nil, nil
	}
	if !c.UsingPrecompiledSDK {
		return []mkfile.DependencyDesc{xchainModule(c.xdevRoot)}, nil
	}
	return []mkfile.DependencyDesc{}, nil
}

func (c *lintCommand) lintPackage(root string) error {
	wd, _ := os.Getwd()
	err := os.Chdir(root)
	if err != nil {
		return err
	}
	defer os.Chdir(wd)
	xcache, err := c.xdevCacheDir()
	if err != nil {
		return err
	}

	err = os.MkdirAll(xcache, 0755)
	if err != nil {
		return err
	}

	err = c.parsePackage(".", xcache)
	if err != nil {
		return err
	}

	if c.makeFileOnly {
		return c.builder.GenerateMakeFile(os.Stdout)
	}

	if c.genCompileCommand {
		cfile, err := os.Create("compile_commands.json")
		if err != nil {
			return err
		}
		c.builder.GenerateCompileCommands(cfile)
		cfile.Close()
	}

	makefile, err := os.Create(".Makefile")
	if err != nil {
		return err
	}
	err = c.builder.GenerateMakeFile(makefile)
	if err != nil {
		makefile.Close()
		return err
	}
	makefile.Close()
	defer os.Remove(".Makefile")

	runner := mkfile.NewRunner(c.ccImage).
		WithEntry(c.entryPkg).
		WithCacheDir(xcache).
		WithXROOT(c.xdevRoot).
		WithOutput(c.output).
		WithMakeFlags(strings.Fields(c.makeFlags))

	if c.compiler != "docker" {
		runner = runner.WithoutDocker()
	}

	if !c.UsingPrecompiledSDK {
		runner = runner.WithoutPrecompiledSDK()
	}

	err = runner.Make(".Makefile")
	if err != nil {
		return err
	}
	return nil
}

// 拷贝文件构造一个工程的目录结构，编译工程
func (c *lintCommand) lintFiles(files []string) error {
	cli, err := client.NewClientWithOpts(client.FromEnv)
	cli.NegotiateAPIVersion(context.Background())

	if err != nil {
		return err
	}
	cli.VolumeCreate(context.TODO(), volume.VolumeCreateBody{})
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	containerCreateBody, err := cli.ContainerCreate(
		context.Background(),
		&container.Config{
			Image:        "chenfengjin.bcc-szwg.baidu.com:8002/xlinter-cpp:latest",
			AttachStdin:  true,
			AttachStdout: true,
			AttachStderr: true,
			Tty:          true,
			WorkingDir:   cwd,

			// TODO
			Cmd: []string{"clang-tidy", "-checks='-*,misc-smart-contract-*'", files[0]},
		},
		&container.HostConfig{
			Mounts: []mount.Mount{
				{
					Source: cwd,
					Target: cwd,
					Type:   mount.TypeBind,
				},
			},
		},
		&network.NetworkingConfig{},
		fmt.Sprintf("xlinter-cpp-%d", time.Now().UnixNano()),
	)
	if err != nil {
		return err
	}

	// TODO
	// defer cli.ContainerRemove(context.Background(), containerCreateBody.ID, types.ContainerRemoveOptions{})

	if err := cli.ContainerStart(
		context.Background(),
		containerCreateBody.ID,
		types.ContainerStartOptions{},
	); err != nil {
		return err
	}
	statusCh, errCh := cli.ContainerWait(context.Background(), containerCreateBody.ID, container.WaitConditionNotRunning)
	select {
	case err := <-errCh:
		if err != nil {
			panic(err)
		}
	case <-statusCh:
	}
	out, err := cli.ContainerLogs(context.Background(), containerCreateBody.ID, types.ContainerLogsOptions{ShowStdout: true})
	if err != nil {
		panic(err)
	}

	io.Copy(os.Stdout, out)
	return nil
}

func init() {
	addCommand(newLintCommand)
}
