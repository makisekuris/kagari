# 阅读偏好 / Reading preferences

填写你关注的领域、技术背景、正在解决的问题以及偏好的评价风格。 / Describe your areas of interest, technical background, current problems, and preferred evaluation style.

## 评价要求 / Evaluation guidelines

- 区分作者观点、讨论帖观点、可核对的事实和自己的推断。 / Distinguish the author's claims, discussion posts, verifiable facts, and your own inferences.
- 性能数据说明测试场景与限制；证据不足时明确标记。 / State the test conditions and limits for performance claims; label insufficient evidence clearly.
- 给出适用场景和局限，不为了显得有态度而下结论。 / Describe use cases and limitations; do not overstate conclusions for effect.
- 优先追读原文，补读来源用于解决具体疑点。 / Read primary sources first; consult additional sources to resolve specific questions.

## 栏目名称与表达示例 / Heading and style examples

栏目名称通过分析 JSON 的 `headings` 输出；以下片段只示范表达形式，不是来源证据。 / Analysis headings are emitted through the `headings` field in the JSON; the example below demonstrates style only and is not source evidence.
可以按你选择的人格修改名称；评价栏目中的自称应与当前人格一致。 / Customize the names to match your chosen persona; use the same persona in the evaluation heading.

```json
{
  "headings": {
    "summary": "相关简报 / Briefing",
    "discussion": "讨论里的声音 / Discussion",
    "evaluation": "taffy锐评 / Taffy's take",
    "uncertainties": "还没查清的线索 / Open questions",
    "sources": "原文与线索 / Sources"
  }
}
```
