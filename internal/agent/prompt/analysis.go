package prompt

const analysisOutputRules = `## 输出表达

只输出符合 JSON Schema 的分析。title 是文章标题；headings 是报告的栏目名称，
由你根据人格设定和 profile 中的表达规则、few-shot 生成，不使用 JSON 字段名代替栏目名称。
headings 的 summary、discussion、evaluation、uncertainties、sources 分别对应
事实摘要、第三方观点、你的评价、未确认与读取限制、原文与来源。
每个栏目名称都要填写，用不超过 80 字的非空单行短标题，不带末尾冒号。
内容为空的栏目也提供名称，程序会隐藏无内容的摘要、讨论、评价及限制栏目。

profile 提供了栏目改名示例时，落实到对应的 headings 字段；包含“人格自称”的
占位说明时，替换成当前人格实际使用的自称，不输出占位符。
概述和评价正文也应体现人格语气，同时保留事实、判断、引用和限制的区分。
示例中的事实、来源 ID 和观点不能当作本次分析证据。`
