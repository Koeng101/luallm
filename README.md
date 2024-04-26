# luallm

This repo is a proof-of-concept for a tool-only LLM. The idea is to force an LLM system designed to write code to ONLY write lua code, which is then immediately executed in a code sandbox. This has several advantages over traditional function calling or tools:

* Tools can be code (lua injected into sandbox) or APIs
* Tool input/output does not need to be read by the context window and can be seamlessly piped using code
* Secondary LLM input/output can be used as a function
* No secondary servers are needed for the lua environment: everything is self-contained

## Why did you build this?
I've been playing with the idea of building an LLM system to do synthetic DNA design. The biggest problem I encountered was that sequences are very large, compared to a context window, and it was difficult to prevent LLM systems like GPT-4 from attempting to read the files upon tool input / output. There were also times where I would want to combine both code and APIs (for example, search for a uniprot protein, download it, then codon optimize). The amount of hand-holding GPT-4 required for this task was impractical.

So, this would solve the biggest problem of an LLM system that could consume human instructions, then write code to operate functions on large amounts of data.
