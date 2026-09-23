项目：go-task-task-a451a546-aad9-4ded-8733-b2ca36714d05｜Task 任务图断点检查点与跨进程恢复
复现验证：go test -count=1 ./...；go test -count=5 -run 'TestCheckpoint(InterruptAndResume|FailfastCancelledNodesRerun|DefinitionChangeInvalidatesAffectedPart|SourceChangeInvalidatesNode|DeferredCommandRunsOnInterrupt)$' .
原始结果：全包测试通过；指定中断、failfast、定义/源文件变化及延迟清理交错用例重复五次均通过。

Rubric ID：1｜状态：通过｜期望行为：显式断点恢复读取同一任务图检查点，关闭时不读写检查点｜实际观察：中断后 --resume 恢复已完成节点，未开启路径不创建检查点目录｜直接证据：checkpoint.go:18-24；checkpoint_integration_test.go:98-160。
Rubric ID：2｜状态：通过｜期望行为：检查点绑定根调用、解析定义、有效变量及源文件身份｜实际观察：不同调用变量不恢复；定义或源文件变化只使受影响分支失效｜直接证据：checkpoint.go:27-50,114-169；internal/checkpoint/checkpoint.go 的 Binding、DefHash、SourcesHash；checkpoint_integration_test.go:225-333,376-387。
Rubric ID：3｜状态：通过｜期望行为：命令、defer、产出物和状态均完成后才记录节点完成｜实际观察：defer 在节点返回前执行；缺失声明产出物时校验失败且节点不记完成；成功路径在校验后调用 Completed｜直接证据：task.go:362-394,408-432；checkpoint.go:62-90；checkpoint_integration_test.go:334-358,498-520。
Rubric ID：4｜状态：通过｜期望行为：恢复跳过有效完成节点，失败、中断、取消节点及受影响后继重新运行｜实际观察：中断恢复重跑中断节点与后继并恢复未变依赖；失败和 failfast 取消节点重新执行｜直接证据：checkpoint.go:94-111,114-184；task.go:292-309,400-416；checkpoint_integration_test.go:98-123,162-223。
Rubric ID：5｜状态：通过｜期望行为：定义、变量或源文件变化只使受影响节点及后继失效｜实际观察：变化分支重跑，未变化依赖显示 restored from checkpoint｜直接证据：checkpoint.go:114-184 通过 DefHash、SourcesHash 和依赖重跑状态准入；checkpoint_integration_test.go:225-333,389-496。
Rubric ID：6｜状态：通过｜期望行为：无法验证的检查点从头执行｜实际观察：调用变量不一致的检查点不恢复节点；内部校验失败时 Manager 不可用｜直接证据：internal/checkpoint/checkpoint.go:112-133,191-223；checkpoint_integration_test.go:376-387。
Rubric ID：7｜状态：通过｜期望行为：并行/failfast 收尾不发布半完成节点，失败或取消节点可重跑｜实际观察：failfast 后未完成节点在恢复时重跑，检查点写入采用临时文件和 rename｜直接证据：task.go:400-432,445-490；internal/checkpoint/checkpoint.go:164-175,246-274；checkpoint_integration_test.go:193-223。
Rubric ID：8｜状态：未通过 成功运行删除检查点失败仍返回成功且遗留记录可被恢复复用｜期望行为：成功完成后旧检查点退出复用范围｜实际观察：删除失败时日志记录 unable to remove checkpoint 后 runFinished 返回 nil；同一 Binding 的遗留文件仍被加载为 usable，后续 --resume 跳过已完成节点｜直接证据：task.go:125-140；internal/checkpoint/checkpoint.go:117-133,197-205｜原因：cp.Discard() 错误只写日志且未传回成功运行主流程，load 对同一 Binding 的保留文件设置 usable=true｜影响：成功结束后的旧完成记录在同一任务调用下仍会被恢复并跳过任务｜证据位置：task.go:137-140；internal/checkpoint/checkpoint.go:117-133,197-205。
Rubric ID：9｜状态：通过｜期望行为：未开启断点模式时依赖、指纹、并行、取消和输出保持兼容｜实际观察：未启用模式的集成用例成功执行且 .task/checkpoint 不存在｜直接证据：checkpoint.go:18-24 的 opt-in 条件；checkpoint_integration_test.go:125-160。
