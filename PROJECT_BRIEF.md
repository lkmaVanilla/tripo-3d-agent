# Tripo Agent

## Goal

一个可公开演示的3D Agent项目

## Product Hypothesis

用户使用自然语言描述3D资产需求。单个Agent负责理解资产用途和限制、规划生产流程、调用Tripo API、跟踪异步任务、验证产物并向用户解释结果。

## Required Capabilities

- Agent Runtime
- Context management
- Intent understanding
- Skills and prompt engineering
- Workflow orchestration
- Tool calling
- Async task handling
- Execution tracing
- Automated evaluation
- Minimal WebSocket progress delivery
- Minimal 3D result viewer

## Technology Constraints

- Backend: Go
- Agent framework: CloudWeGo Eino
- 3D generation: Tripo API
- Architecture: Single Agent
- Frontend: Minimal implementation only

## Explicit Non-goals

- Multi-Agent
- General-purpose Agent platform
- Kubernetes
- GPU or inference infrastructure
- RAG
- Complex frontend
- Training or fine-tuning 3D models
- Building a replacement for Tripo Studio

## MVP Success

A user can submit one asset request and see:

1. Parsed asset intent
2. Agent plan
3. Tripo tool calls
4. Asynchronous progress
5. Generated 3D result
6. Basic asset validation
7. Complete execution trace
8. Evaluation result
