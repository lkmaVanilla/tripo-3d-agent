#!/usr/bin/env python3
"""离线汇总一次完整评测；不请求模型，不修改原始记录或已有汇总。

    python3 scripts/summarize-conversation-evaluation.py EVIDENCE_DIR
    python3 scripts/summarize-conversation-evaluation.py EVIDENCE_DIR --output NEW_FILE
    python3 scripts/summarize-conversation-evaluation.py EVIDENCE_DIR --check-against SUMMARY

校验 manifest 声明的 20 + 8 场景、每例 3 次，共 84 个目标。
semantic-review.json 必须逐个覆盖完整目标，
每个维度提供 passed/failed，只有经审核的未知提交解释可用 not_applicable。
自动失败与语义失败取交集，正式结果或 Runtime 恢复不能覆盖原始提议的失败。
technical-only-v1 批次与人工审核必须声明相同评分标准；不重新解释旧批次的判分。
"""

import argparse
import json
import re
import sys
from pathlib import Path


DIMENSIONS = ("intent", "strategy", "explanation")
CONSTRAINT_DIMENSIONS = {
    "intent_numeric_hard_constraints": "intent",
    "production_budget_proposal": "strategy",
    "proposal_face_limit": "strategy",
    "delivery_has_current_run_evidence": "strategy",
    "answer_hides_no_production": "strategy",
}
SUPPORT_FILES = {"manifest.json", "semantic-review.json", "summary.json"}
RAW_RECORD_FIELDS = {"case", "snapshot", "run_id", "repeat", "response_metadata",
                     "automated_checks", "automated_passed", "critical_errors_automated", "suite_acceptance"}
SCOPE = "当前v3；每目标每维度一个适用综合项；自动事实与全提议语义审核共同判分"
TECHNICAL_ONLY_RUBRIC = "technical-only-v1"
TECHNICAL_ONLY_SCOPE = (
    "当前v3；仅评测非视觉的意图、策略与解释；不评测外观、视觉符合度或视觉声明；"
    "每目标每维度一个适用综合项；自动事实与全提议人工语义审核共同判分"
)


class EvidenceError(ValueError):
    """证据或审核不完整、不一致，不能生成看似通过的汇总。"""


def require(condition, message):
    if not condition:
        raise EvidenceError(message)


def unique_object(pairs):
    result = {}
    for key, value in pairs:
        require(key not in result, f"JSON 对象包含重复键：{key}")
        result[key] = value
    return result


def load_json(path):
    try:
        return json.loads(path.read_text(encoding="utf-8"), object_pairs_hook=unique_object)
    except (OSError, ValueError) as error:
        raise EvidenceError(f"{path.name}: {error}") from error


def nonnegative_integer(value, label):
    require(type(value) is int and value >= 0, f"{label} 必须为非负整数")
    return value


def same_keys(actual, expected, label):
    missing, extra = sorted(expected - actual), sorted(actual - expected)
    require(not missing and not extra, f"{label}：缺失={missing}；多余={extra}")


def raw_record_names(directory):
    """保留分组审核底稿；命名不能把实际案例伪装成辅助记录。"""
    names = set()
    for path in directory.glob("*.json"):
        if path.name in SUPPORT_FILES:
            continue
        if path.name == "run-observation.json" or re.fullmatch(r"semantic-.+-review\.json", path.name):
            document = load_json(path)
            if isinstance(document, dict) and not RAW_RECORD_FIELDS.intersection(document):
                # 分组底稿与批次运行审计都可保留，但不参与计分或替代完整审核。
                continue
        names.add(path.name)
    return names


def expected_records(manifest):
    repetitions = nonnegative_integer(manifest.get("repetitions"), "manifest.repetitions")
    cases = manifest.get("cases")
    require(repetitions > 0 and isinstance(cases, list) and cases, "manifest 没有完整案例清单")
    expected = {}
    for case in cases:
        for field in ("Suite", "ID"):
            require(isinstance(case.get(field), str) and re.fullmatch(r"[A-Za-z0-9_-]+", case[field]),
                    f"manifest 案例 {field} 不是合法标识")
        require(case["Suite"] != "all", "suite 名 all 保留用于整体统计")
        for repeat in range(1, repetitions + 1):
            name = f"{case['Suite']}-{case['ID']}-{repeat}.json"
            require(name not in expected, f"manifest 案例重复：{name}")
            expected[name] = (case, repeat)
    return expected


def evaluation_rubric(manifest, reviewed):
    """旧批次保持原判分；新标准必须由批次与人工审核共同显式声明。"""
    if "evaluation_rubric" not in manifest:
        require("evaluation_rubric" not in reviewed,
                "旧批次 manifest 未声明 evaluation_rubric，不能混用新评分标准的语义审核")
        return None
    rubric = manifest["evaluation_rubric"]
    require(rubric == TECHNICAL_ONLY_RUBRIC, f"未知 evaluation_rubric：{rubric}")
    require(reviewed.get("evaluation_rubric") == rubric,
            "manifest 与 semantic-review.json 的 evaluation_rubric 缺失或不一致")
    return rubric


def check_dimension(check):
    dimension = check.get("Dimension")
    if dimension == "constraint":
        require(check.get("Name") in CONSTRAINT_DIMENSIONS,
                f"约束检查尚未明确映射维度：{check.get('Name')}")
        return CONSTRAINT_DIMENSIONS[check["Name"]]
    require(dimension in DIMENSIONS, f"未知自动检查维度：{dimension}")
    return dimension


def reviewed_dimensions(review, record):
    require(isinstance(review.get("reason"), str) and review["reason"].strip(), "缺少逐例语义审核依据")
    result = {}
    for dimension in DIMENSIONS:
        value = review.get(dimension)
        require(value in ("passed", "failed", "not_applicable"), f"{dimension} 缺少已完成的语义审核")
        if value == "not_applicable":
            # 不能仅凭运行失败或缺少说明减少分母；必须有明确审核标记和未知提交事实。
            session = record["snapshot"]["session"]
            outcome = session.get("result") or {}
            require(dimension == "explanation" and outcome.get("reason") == "submission_unknown"
                    and outcome.get("source") == "runtime" and record["case"].get("Provider") == "unknown",
                    "not_applicable 仅限 Runtime 直接结束未知提交、模型未获得解释机会的解释项")
            events = record["snapshot"]["events"]
            submits = [e["seq"] for e in events if e.get("kind") == "tool_submitting"]
            require(submits and not any(e.get("kind") == "agent_proposal" and e["seq"] > max(submits)
                                       for e in events), "未知提交后存在模型响应，不能豁免解释维度")
            result[dimension] = None
        else:
            result[dimension] = value == "passed"
    return result


def summarize_record(name, record, case, repeat, review):
    require(record.get("case") == case and record.get("repeat") == repeat,
            "原始案例或重复编号与 manifest 不一致")
    snapshot, run_id = record["snapshot"], record.get("run_id")
    session, events = snapshot["session"], snapshot["events"]
    require(isinstance(run_id, str) and run_id and session.get("id") == run_id, "目标 Run 身份不一致")
    require(isinstance(events, list), "缺少单 Run 原始事件")
    sequences = [e["seq"] for e in events]
    require(sequences == sorted(set(sequences)), "单 Run 事件序号重复或乱序")
    model_calls = sum(e.get("kind") == "model_call" for e in events)
    require(model_calls == session.get("model_calls"), "model_call 事件数与模型请求计数不一致")
    initial = nonnegative_integer(case.get("InitialProduction"), "InitialProduction")
    production = nonnegative_integer(session.get("production"), "production")
    submissions = production - initial
    require(submissions >= 0 and submissions == sum(e.get("kind") == "tool_submitting" for e in events),
            "扣除 InitialProduction 后的提交数与本次实际提交事件不一致")

    dimensions = reviewed_dimensions(review, record)
    checks = record.get("automated_checks")
    require(isinstance(checks, list) and checks, "缺少原始自动检查")
    critical, actual_counts = [], []
    for check in checks:
        require(type(check.get("Passed")) is bool and type(check.get("Critical")) is bool,
                "自动检查缺少 Passed/Critical 布尔值")
        dimension = check_dimension(check)
        if check.get("Name") == "allowed_submission_count":
            actual = re.fullmatch(r"actual=(\d+) expected=\d+\.\.\d+", check.get("Detail", ""))
            require(actual is not None, "缺少固定 Provider 的实际提交数量")
            actual_counts.append(int(actual.group(1)))
        if not check["Passed"]:
            require(dimensions[dimension] is not None, f"{dimension} 存在自动失败，不能标记为不适用")
            dimensions[dimension] = False
            if check["Critical"]:
                critical.append(f"automated:{check['Name']}:{check.get('Detail', '')}")
    require(actual_counts == [submissions], "固定 Provider 计数与生产状态不一致或重复")
    automated_passed = all(check["Passed"] for check in checks)
    require(record.get("automated_passed") is automated_passed, "automated_passed 与逐条检查不一致")
    require(record.get("critical_errors_automated") == len(critical), "自动关键错误总数与逐条检查不一致")
    semantic_critical = review.get("critical_errors")
    require(isinstance(semantic_critical, list) and all(isinstance(x, str) and x.strip() for x in semantic_critical),
            "语义关键错误必须逐条列出（无错误时为空数组）")
    critical.extend("semantic:" + error for error in semantic_critical)

    metadata = record.get("response_metadata")
    require(isinstance(metadata, list), "缺少原始模型响应元数据；不能推算 token 数")
    require(len(metadata) <= model_calls, "响应数超过模型请求数")
    proposals = [event["data"] for event in events if event.get("kind") == "agent_proposal"]
    require(len(metadata) == len(proposals), "响应元数据与原始模型提议数量不一致")
    response_indexes = [item.get("message_index") for item in metadata]
    require(all(type(index) is int and index >= 0 for index in response_indexes)
            and response_indexes == sorted(set(response_indexes)), "模型响应索引缺失、重复或乱序")
    tokens = {"prompt": 0, "completion": 0}
    for item, proposal in zip(metadata, proposals):
        response = item.get("response_meta") or {}
        usage = (item.get("response_meta") or {}).get("usage")
        require(isinstance(usage, dict), "响应缺少 usage，不能将未知 token 数记为零")
        require(usage == proposal.get("usage") and response.get("finish_reason") == proposal.get("finish_reason"),
                "响应 usage/finish_reason 与原始提议不一致")
        for kind in tokens:
            tokens[kind] += nonnegative_integer(usage.get(kind + "_tokens"), kind + "_tokens")
    return {
        "file": name, "suite": case["Suite"], "case": case["ID"], "repeat": repeat,
        "run_id": run_id, "status": session["status"], "model_calls": model_calls,
        "actual_provider_submissions": submissions, "automated_passed": automated_passed,
        "dimensions": dimensions, "critical_errors": critical, "reason": review["reason"],
        "length_terminated_responses": [item for item in metadata
                                        if (item.get("response_meta") or {}).get("finish_reason") == "length"],
        "recorded_response_count": len(metadata), "recorded_prompt_tokens": tokens["prompt"],
        "recorded_completion_tokens": tokens["completion"],
    }


def aggregate(rows, thresholds):
    counts = {
        "runs": len(rows), "raw_automated_passed": sum(row["automated_passed"] for row in rows),
        "model_calls": sum(row["model_calls"] for row in rows),
        "actual_provider_submissions": sum(row["actual_provider_submissions"] for row in rows),
        "critical_errors": sum(len(row["critical_errors"]) for row in rows), "dimensions": {},
    }
    for dimension in DIMENSIONS:
        values = [row["dimensions"][dimension] for row in rows if row["dimensions"][dimension] is not None]
        counts["dimensions"][dimension] = {"passed": sum(values), "applicable": len(values),
                                           "rate": sum(values) / len(values) if values else None}
    for field in ("recorded_response_count", "recorded_prompt_tokens", "recorded_completion_tokens"):
        counts[field] = sum(row[field] for row in rows)
    passed = counts["critical_errors"] <= thresholds["critical_errors"]
    for dimension, score in counts["dimensions"].items():
        passed = passed and score["rate"] is not None and score["rate"] >= thresholds[dimension + "_pass_rate"]
    counts["status"] = "passed" if passed else "not_passed"
    return counts


def summarize(directory):
    directory = Path(directory)
    manifest = load_json(directory / "manifest.json")
    require(manifest.get("evaluation_profile") == "current-v3", "此汇总器只处理 current-v3 独立评测批次")
    expected = expected_records(manifest)
    suite_sizes = {suite: sum(case["Suite"] == suite for case in manifest["cases"])
                   for suite in {case["Suite"] for case in manifest["cases"]}}
    require(manifest["repetitions"] == 3 and suite_sizes == {"baseline-current-v3": 20, "conversation-v3": 8},
            "manifest 必须包含当前 v3 的 20 + 8 场景、每例 3 次，共 84 个目标")
    actual = raw_record_names(directory)
    same_keys(actual, set(expected), "原始记录覆盖不完整")
    reviewed = load_json(directory / "semantic-review.json")
    rubric = evaluation_rubric(manifest, reviewed)
    require(isinstance(reviewed.get("reviewer"), str) and reviewed["reviewer"].strip(), "缺少审核者")
    require(isinstance(reviewed.get("reviewed_at"), str) and reviewed["reviewed_at"].strip(), "缺少审核时间")
    reviews = reviewed.get("reviews")
    require(isinstance(reviews, dict), "缺少逐例语义审核 reviews")
    same_keys(set(reviews), set(expected), "语义审核覆盖不完整")
    thresholds = manifest.get("thresholds", {})
    require(type(thresholds.get("critical_errors")) is int and thresholds["critical_errors"] == 0,
            "本评测要求关键错误为零")
    for dimension in DIMENSIONS:
        value = thresholds.get(dimension + "_pass_rate")
        require(type(value) in (int, float) and 0 <= value <= 1, f"缺少合法的 {dimension} 门槛")
        if rubric == TECHNICAL_ONLY_RUBRIC:
            require(value == 0.9, f"technical-only-v1 的 {dimension} 门槛必须为 90%")
    rows = []
    for name, (case, repeat) in sorted(expected.items()):
        try:
            rows.append(summarize_record(name, load_json(directory / name), case, repeat, reviews[name]))
        except (KeyError, TypeError, EvidenceError) as error:
            raise EvidenceError(f"{name}: {error}") from error
    require(len({row["run_id"] for row in rows}) == len(rows), "原始记录复用了同一目标 Run")
    suites = sorted({row["suite"] for row in rows})
    counts = {suite: aggregate([row for row in rows if row["suite"] == suite], thresholds) for suite in suites}
    counts["all"] = aggregate(rows, thresholds)
    # 总平均不能掩盖任何单独场景组未达标。
    status = "passed" if all(value["status"] == "passed" for value in counts.values()) else "not_passed"
    result = {"scope": SCOPE, "status": status, "counts": counts, "rows": rows}
    if rubric is not None:
        result["scope"] = TECHNICAL_ONLY_SCOPE
        result["evaluation_rubric"] = rubric
    return result


def main(argv=None):
    parser = argparse.ArgumentParser(description=__doc__, formatter_class=argparse.RawDescriptionHelpFormatter)
    parser.add_argument("directory", type=Path, help="完整独立评测目录")
    parser.add_argument("--output", type=Path, help="新建汇总文件；拒绝覆盖任何已有文件，默认输出到标准输出")
    parser.add_argument("--check-against", type=Path, help="与现有汇总进行完整 JSON 等值核对，不修改对方")
    args = parser.parse_args(argv)
    try:
        summary = summarize(args.directory)
        if args.check_against:
            require(summary == load_json(args.check_against), "计算结果与指定汇总不一致；未修改已有证据")
            print("全部分组计数、逐例维度、关键项及响应统计与既有汇总一致。", file=sys.stderr)
        output = json.dumps(summary, ensure_ascii=False, indent=2, allow_nan=False) + "\n"
        if args.output:
            with args.output.open("x", encoding="utf-8") as file:
                file.write(output)
        else:
            sys.stdout.write(output)
    except (EvidenceError, OSError, TypeError, KeyError) as error:
        print(f"无法汇总：{error}", file=sys.stderr)
        return 2
    return 0


if __name__ == "__main__":
    sys.exit(main())
