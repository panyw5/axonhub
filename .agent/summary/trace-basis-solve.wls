(* Generic linear trace-system solver.
   Inputs must already be expanded using the source's linearity/OPE rules.
   Keep traces unrenamed until after solving. Coefficients are treated
   generically; functional identities between coefficients must be supplied
   by the caller. This helper does not generate missing VOA relations.

   result = TraceBasisSolve[allResiduals, s /@ (o /@ basis), targets];
   result["Reduced"] /. basisRename

   allResiduals accepts expressions equal to zero, or explicit equalities.
   targets is a list of traces or linear combinations of traces.
   BasisOnly checks the requested targets, not every auxiliary variable. *)

Clear[TraceAtoms, TraceBasisSolve];
TraceAtoms[expr_] := DeleteDuplicates[
  Cases[{expr}, HoldPattern[s[o[_]]], Infinity]
];

TraceBasisSolve[equations_List, keep_List, targets_List] := Module[
  {residuals, unknowns, solutions, rules, reduced, remaining, checks},
  residuals = DeleteCases[Expand[Flatten[equations] /.
    {Equal[l_, r_] :> l - r, True -> 0, False -> 1}], 0];
  unknowns = Complement[TraceAtoms[{residuals, targets}], keep];
  If[unknowns === {},
    rules = {},
    solutions = Quiet[Solve[Thread[residuals == 0], unknowns], Solve::svars];
    If[!MatchQ[solutions, {{___Rule} ..}],
      Return[Failure["NoGenericSolution", <|
        "MessageTemplate" ->
          "No generic rule solution. Check consistency, basis constraints, and coefficient identities.",
        "SolveResult" -> solutions, "Unknowns" -> unknowns|>]]
    ];
    If[Length[solutions] =!= 1,
      Return[Failure["MultipleBranches", <|"SolveResult" -> solutions|>]]
    ];
    rules = First[solutions]
  ];
  reduced = Simplify[targets /. rules];
  remaining = Complement[TraceAtoms[reduced], keep];
  checks = Simplify[residuals /. rules];
  <|"Rules" -> rules, "Reduced" -> reduced,
    "Unknowns" -> unknowns, "Unresolved" -> remaining,
    "BasisOnly" -> (remaining === {}),
    "Residuals" -> checks,
    "Verified" -> AllTrue[checks, TrueQ[# == 0] &]|>
];
