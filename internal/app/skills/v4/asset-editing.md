---
name: asset-editing
description: 明确历史输入版本和后续加工条件
---

初始历史加工输入以 input_binding.status=selected 和 selected_version_id 为准。背景中看得到版本或报告、当前预览、历史只有一个模型，都不等于用户选中。status=not_selected 时先 ask_user 要求明确选择，不能先 set_intent 再补澄清；用户明确不选择则 finish_request(outcome="answer")，零生产结束。绑定后用真实版本ID调用 decimate_asset。此规则不要求为本Run新产物另选历史输入；原生成目标的候选纠偏仍可直接加工本Run有效产物。

Go核对归属、文件、摘要和技术有效性，再将保存的GLB内部上传给Tripo；这不开放用户导入。输入准备失败保留旧版，不能默换其他版本。goal_kind 为 decimate 时不能用生成替代；generate / regenerate 中的候选减面只是纠偏操作，后续选择仍按 correction 和原目标判断。

仅减面目标：仅在reduction_mode=within_limit且 input_assessment 显示已有输入满足本目标技术要求时，调用 finish_request(outcome="answer") 说明无需加工，零生产、零新版本。明确重新生成目标仍可生成新版本。每轮只调用一个工具或一个 Skill，不能同时加载与加工；结果或限制都要通过 finish_request 收尾。

新加工输出追加版本并记录真实父关系，旧文件和报告保留。input_version.report 是来源版本的技术证据，input_assessment 按本次上限重评；两者都不证明外观或用途符合。来源意图记录的是旧制作要求，不能据此声称版本已经呈现相应风格或材质。解释加工结果时使用对应版本实测数字，明确未做视觉检查，不将未核验的外观当成事实。

reduction_mode=further时输入通过上限检查也必须进一步降低；选择比加工输入实测面数小的合法目标。无合法目标时解释当前工具限制，零生产结束。输出report.passed与goal_satisfied分开：只有实际少于初始输入且适用技术检查通过才完成进一步减面。不能把所选目标数字增加为用户上限。
