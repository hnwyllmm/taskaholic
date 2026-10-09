package store

import "strings"

const seekDBBuildGuidance = `SeekDB 仓库构建策略（Manager 提供的可信执行指引）：
- 在 Linux 开发环境中先运行 command -v ob-make；可用时优先使用 ob-make，利用已配置的 ccache 和 distcc。首次诊断可运行 ob-make --plugin-status，但不要反复执行。
- 仓库官方 build.sh 仍负责配置正确的工具链和 CMake 参数；不要自行拼接 CMake、编译器或链接参数。配置完成后，优先用 ob-make 驱动已有构建目录。
- 修改少量 C++ 文件后的快速检查优先使用 ob-make --inc -s <源文件>；验证当前增量改动优先使用 ob-make --inc，Release 增量验证使用 ob-make --inc --release。
- 已有 build_release、build_sanity 等目录时，优先使用 ob-make -C <构建目录> -j<合理并发数> <受影响目标>。ob-make --inc 默认并发为 32；结合当前机器负载和同时运行的任务选择并发，不要在没有资源证据时保守降为 -j1 或 -j4。
- 不删除或重新初始化仍可复用的构建目录。实施中先做受影响目标和测试的增量验证；代码稳定后才执行批准 validation_plan 中必要的完整 Release、Sanity、ASAN 或 Bazel 门禁，不要每次小改动都重跑整套矩阵。
- ob-make 不可用、失败或不适用于目标时可以回退仓库官方构建入口；先记录实际错误和回退理由，不得因该建议阻塞任务，也不得绕过已批准验证门禁。`

func repositoryBuildGuidance(repository string) string {
	repository = strings.ToLower(strings.TrimSpace(repository))
	repository = strings.TrimSuffix(repository, "/")
	repository = strings.TrimSuffix(repository, ".git")
	for _, prefix := range []string{"https://github.com/", "http://github.com/", "ssh://git@github.com/", "git@github.com:"} {
		repository = strings.TrimPrefix(repository, prefix)
	}
	if repository == "oceanbase/seekdb" {
		return seekDBBuildGuidance
	}
	return ""
}
