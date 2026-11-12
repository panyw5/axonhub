(* Run after the definitions and singwt0v1,...,singwt0v9 in
   D4ZhusRecursionCheck.wls have been evaluated.
   This file does not change omega or overwrite the existing solution rules.
   The explicit formula below is for omega = 3 h1 + 5 h2 + 3 h3 + 3 h4.
   All derivatives here use the original script's Zhu-coordinate convention. *)

h12Reduction = Module[
  {target, nv, roots, coeffs, eq, rule, rename, expected, positive, en},
  target = s[o[NO[hh[1], hh[2]]]];
  nv = OPESimplify[singwt0v1 - singwt0v6 - singwt0v9];
  roots = {{0, 1, -1, 0}, {0, 1, 0, -1}, {0, 1, 0, 1},
    {0, 1, 1, 0}, {1, 0, -1, 0}, {1, 0, 0, -1},
    {1, 0, 0, 1}, {1, 0, 1, 0}};
  coeffs = {-3, -1, -1, 1, 3, 1, 1, -1};
  rename = {s[o[One]] -> S0, s[o[hh[1]]] -> S1,
    s[o[hh[2]]] -> S2, s[o[hh[3]]] -> S3, s[o[hh[4]]] -> S4,
    s[o[NO[hh[1], hh[1]]]] -> S11};

  (* Each summand vanishes. The combination cancels all root bilinears
     and the derivative, leaving only target and the six chosen traces. *)
  eq = s[o[nv]] - 2 Identity52[hh[1], One]
    + 2 Pi I Sum[coeffs[[j]] Identity54[ff[roots[[j]]], ee[roots[[j]]]],
      {j, Length[roots]}];
  eq = Expand[(eq //. $DefaultCBRules) /.
    EEminus[0][_, _] -> -1] /. rename;
  rule = First[Solve[eq == 0, target]];

  (* E_n(1/y^r) = (-1)^n E_n(y^r), in the source's EEE notation. *)
  positive = Expand[(target /. rule) /.
    EEminus[n_][y^p_, q] :> (-1)^n EEE[n][{{1}, {y^(-p)}}][q]];
  en[n_, r_] := EEE[n][{{1}, {y^r}}][q];
  expected = -S11/2 + Pi I (
      3 S2 en[1, 1]
      - (3 S1 + S2 - S3 - S4) en[1, 2]
      - (2 S1 + 3 S2 + 2 S3 + 2 S4) en[1, 3]
      + (S1 + S2 + S3 + S4) en[1, 4])
    + 4 Pi^2 S0 (3 en[2, 1] - en[2, 2] - 3 en[2, 3] + en[2, 4]);
  <|"NullVectorCombination" -> nv, "Equation" -> eq,
    "RawRules" -> rule, "Rules" -> {target -> expected},
    "IdentityCheck" -> Simplify[eq /. rule],
    "ExplicitFormulaCheck" -> Simplify[positive - expected]|>
];

h12sol = h12Reduction["Rules"];
(* Apply h12sol to the already reduced Dys expression.
   Both checks in h12Reduction should equal 0. *)
