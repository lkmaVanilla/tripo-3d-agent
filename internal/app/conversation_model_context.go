package app

// conversationModelView 在已有业务事实旁明确解释边界，仅用于模型输入。
// 不复制或重新计算实测报告，不把用户要求、模型假设或供应商错误正文升级为事实。
// HTTP/WS 继续使用 conversationRunView；旧 profile 和 Eino 原始消息协议不变。
func conversationModelView(v Session) map[string]any {
	view := conversationRunView(v)
	view["conversation_context"] = v.ConversationContext
	view["input_version"] = v.InputVersion
	view["input_binding"] = conversationInputBinding(v)
	view["intent_draft"] = v.IntentDraft
	view["goal_kind"] = v.GoalKind
	if v.Current != nil {
		if evidence := conversationFailureEvidence(*v.Current); evidence != nil {
			view["failure_evidence"] = evidence
		}
	}
	view["explanation_basis"] = map[string]any{
		// scope 表达检查能力，不表示本次已经完成检查；实测与通过状态仍取对应 report。
		"evidence_scope":         []string{"file_validity", "triangles", "bytes"},
		"visual_status":          "not_checked",
		"provider_failure_cause": "not_diagnosed",
		"parameter_effects":      "not_guaranteed",
		"requirements_role":      "targets_and_assumptions_not_observations",
	}
	return view
}

// conversationInputBinding 区分历史事实和用户输入选择，只投影持久化的真实绑定。
// selected 不是文件可加工或技术合格结论；本 Run 新候选不属于历史初始输入。
func conversationInputBinding(v Session) map[string]any {
	binding := map[string]any{
		"scope": "initial_historical_version", "status": "not_selected", "selected_version_id": "",
		"history_role":  "背景中的版本ID和技术报告可以用于解释，但不代表用户已选择加工输入；不能从历史、预览或只有一个版本推定选择。",
		"decision_rule": "若用户要求加工或参考历史版本且status=not_selected，先ask_user要求版本选择，再set_intent；明确不选择则finish_request(outcome=answer)，零生产。新建文本生成无需历史输入；已接受生成目标对本Run新候选纠偏也无需选择历史输入。",
	}
	if v.InputVersion != nil {
		binding["status"], binding["selected_version_id"] = "selected", v.InputVersion.ID
	}
	return binding
}

// conversationFailureEvidence 从程序保存的错误类型构造证据，而非解析错误正文。
// 工具反馈和下一轮上下文共享此投影，恢复后仍保持相同来源和不确定性。
// 不枚举动态工具权限；是否可继续仍由目标、实时预算、截止时间和工具门禁决定。
func conversationFailureEvidence(op Operation) map[string]any {
	if op.Stage == "done" && op.ErrorCode == "download_resource_limit" {
		return map[string]any{"operation_id": op.ID, "task_id": op.TaskID, "cause": "system_download_limit", "observed": map[string]any{"file_inspection": "not_performed", "download_limit_bytes": 150 << 20}, "decision_basis": "文件超出系统下载保护，未取得完整文件，不能确认实际面数或体积，不属于用户验收上限失败。"}
	}

	if op.Stage != "done" || op.ErrorCode != "missing_model_output" {
		return nil
	}
	return map[string]any{
		"operation_id": op.ID, "task_id": op.TaskID,
		"observed":                      map[string]any{"query_status": "success", "model_url": "missing", "file_inspection": "not_performed"},
		"cause":                         "unknown",
		"prompt_or_parameter_influence": "unknown_not_ruled_out",
		"retry_effect":                  "unknown",
		"decision_basis":                "缺少文件，无法继续检查或对该文件减面。重复出现只增加同类失败次数，不构成根因诊断。若目标、剩余预算、期限和工具前置条件允许，可选择另一次生产尝试，也可主动停止。提前停止应表述为本次选择，不是证明调整参数无效或已无合法路径。",
	}
}
