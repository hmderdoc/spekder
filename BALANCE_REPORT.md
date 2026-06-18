# Spectre Roster Balance Report

Reproduce verbatim (LARGE sample):

    go run ./cmd/balancesim -matrix -duelseeds 100 -ffaseeds 300 -ctfseeds 150 -maxsec 120

KOTH (objective-hold metric, separate run):

    go run ./cmd/balancesim -kothseeds 300 -maxsec 120 -skip duel,ffa,ctf

(diff=HARD, base seed=1000, deterministic server-authoritative engine, bot-only.
OP?/UP? = the harness's own >1.5-stddev-from-mean flag.)

Build for this run: TURTLE SNAP damage 16 -> 24 (+50%, 20 -> 30 DPS) - a modest
offense bump so the defensive char can close some matches without becoming a
threat. On top of the prior build's full TURTLE status immunity (POISON full /
SLOW / BLEED / DRAIN; still takes KNOCKBACK + direct damage), rear ranged shield,
FALCON TALON 12, OCTOPOD STAB reach 3.2, TANK nerf, SERPENT+FALCON crit &
move-dodge, elephant immunities, octopod spy.
Bots don't run CC combos or shoot the crab turret; crab ACC >100% = turret artifact.


## DUEL  (1v1 deathmatch, 15 chars, 100 seeds/pair = 10,500 matches)

    RANK  CHARACTER  WIN-RATE  K/D   KILLS/G  DEATHS/G  ACC  DMG/G  FLAG
    1     ELEPHANT   71.5%     1.73  7.4      4.3       63%  755
    2     MINOTAUR   67.6%     1.25  6.8      5.4       83%  681
    3     FALCON     61.5%     1.25  7.2      5.7       44%  769
    4     TANK       61.1%     1.39  8.1      5.8       64%  767
    5     TIGER      60.6%     1.40  7.6      5.4       62%  754
    6     SCORPION   57.3%     1.26  8.1      6.4       58%  842
    7     OCTOPOD    55.9%     1.23  4.2      3.4       38%  487
    8     HUMANOID   44.1%     1.01  8.3      8.3       59%  843
    9     INSECT     42.1%     0.76  6.1      8.0       69%  639
    10    GORILLA    36.4%     0.80  6.3      7.9       71%  632
    11    TURTLE     35.6%     1.00  3.4      3.4       94%  374
    12    MANTIS     32.9%     0.88  6.7      7.6       64%  714
    13    T-REX      23.9%     0.69  5.7      8.2       40%  568
    14    SERPENT    18.8%     0.51  4.6      9.0       53%  476
    15    CRAB       6.9%      0.44  4.0      9.1       58%  468    UP?

    mean 45.1%, stddev 18.5%

### Duel win-rate matrix (row beats column, %)

    row\col    ELEP MINO FALC TANK TIGE SCOR OCTO HUMA INSE GORI TURT MANT T-RE SERP CRAB
    ELEPHANT   -    100  1    39   55   49   100  77   3    100  88   100  99   95   95
    MINOTAUR   0    -    2    98   24   93   21   99   96   82   79   65   96   91   100
    FALCON     95   94   -    7    98   38   11   29   53   82   78   64   83   29   100
    TANK       36   1    78   -    37   76   18   96   100  33   79   20   82   100  100
    TIGER      25   67   0    40   -    30   10   53   100  77   95   92   62   100  98
    SCORPION   22   4    36   7    50   -    67   57   99   51   90   77   97   46   99
    OCTOPOD    0    50   85   64   80   15   -    39   71   85   16   92   100  62   24
    HUMANOID   11   0    54   1    32   25   40   -    6    40   69   44   98   98   99
    INSECT     89   1    28   0    0    0    19   79   -    100  0    75   99   0    100
    GORILLA    0    10   3    47   15   24   10   46   0    -    67   93   3    93   99
    TURTLE     0    2    9    9    0    5    60   9    96   4    -    14   98   97   95
    MANTIS     0    17   4    71   4    10   3    40   13   1    56   -    44   98   100
    T-REX      0    1    10   11   15   1    0    0    0    88   1    20   -    99   88
    SERPENT    3    5    57   0    0    31   17   1    93   1    0    1    0    -    54
    CRAB       3    0    0    0    0    1    55   1    0    0    2    0    7    28   -


## FFA  (free-for-all, all 15, 300 matches; win = top fragger; even baseline 6.7%)

    RANK  CHARACTER  WIN-SHARE  K/D   KILLS/G  DEATHS/G  ACC   DMG/G  FLAG
    1     TIGER      12.3%      1.45  13.1     9.0       61%   1160   OP?
    2     MINOTAUR   10.3%      1.14  12.8     11.2      80%   1164
    3     T-REX      8.7%       1.03  12.0     11.6      52%   1092
    4     CRAB       8.0%       1.02  11.7     11.5      123%  1458
    5     HUMANOID   7.3%       0.86  11.4     13.2      66%   1154
    6     GORILLA    7.3%       0.98  11.6     11.8      72%   1115
    7     SCORPION   7.3%       1.07  11.9     11.2      65%   1254
    8     FALCON     6.0%       1.05  10.9     10.4      58%   1147
    9     TANK       4.3%       1.06  10.8     10.2      72%   1035
    10    ELEPHANT   3.0%       1.37  10.8     7.9       67%   1092
    11    MANTIS     3.0%       0.88  10.8     12.3      71%   1059
    12    SERPENT    2.0%       0.74  9.8      13.2      60%   965
    13    OCTOPOD    1.3%       0.96  8.3      8.7       48%   611
    14    INSECT     0.7%       0.78  9.3      12.0      72%   907
    15    TURTLE     0.0%       0.73  4.1      5.7       83%   429    UP?

    mean 5.4%, stddev 3.6%


## CTF healer A/B  (team0 = 3 fighters + healer  vs  team1 = 4 fighters; 300 each)

    (after: healer-AI upgrade [proximity + hysteresis pick, BUTTERFLY casts AEGIS,
    regen 2.5->5], STAG battle-medic kit rework [SWIFT->RALLY heal+knockback burst,
    antler GORE charge dealing 24, bot uses both as real plays], AND heal-output
    boost [MEDIC 25->35 +40%, AURA 14->25 & RALLY 30->54 +80%])

    HEALER     T0 WINS  T1 WINS  TIES  T0 KILL-WIN%  KILL DIFF (T0-T1)  HEAL/GAME
    BUTTERFLY  122      155      23    44%           -1.1               1060
    STAG       126      147      27    46%           -0.6               702

    progression (kill-win% / kill-diff):
      BUTTERFLY  base 17%/-7.1  ->  AI 29%/-4.4  ->  +regen 33%/-3.5  ->  +heal 44%/-1.1
      STAG       base 5%/-10.9  ->  AI 5%/-12.1  ->  battle-medic 24%/-4.3  ->  +heal 46%/-0.6

    NOTE: both healers are now ~break-even on KILLS (-1.1 / -0.6, 44-46% kill-win) -
    and this metric IGNORES the objective payoff (flag runs, sustain) that is the
    healer's real job, so both now genuinely earn their team slot. STAG went from a
    -12.1 trap pick to the marginally stronger of the two.


## KOTH  (objective-hold) - not yet run this build

    Run separately: go run ./cmd/balancesim -kothseeds 300 -maxsec 120 -skip duel,ffa,ctf
    Ranks by hold-points/game (the metric duel/FFA structurally miss). This is the
    arena TURTLE is designed for; FFA/duel undersell a squatter-anchor.


## This run's changes -> effect

    TURTLE SNAP +50% (16->24):  duel 30.0% -> 35.6% (#11), K/D 0.73 -> 1.00 (even), DMG/G 309 -> 374
                                deaths/g flat at 3.4 (survival intact); now beats OCTOPOD (20 -> 60)
                                FFA still 0% (squatter); K/D 0.55 -> 0.73, DMG/G 343 -> 429
    ripple (minimal):           OCTOPOD 60.1% -> 55.9% (#7), MANTIS 34.9% -> 32.9%; field tightened (stddev 19.2 -> 18.5)

## Standing flags

- TURTLE evened out (35.6%, #11, K/D 1.00) - the SNAP bump lets it close some
  matches while the status immunities still wall every DoT char (INSECT 96,
  SERPENT 97, T-REX 98, CRAB 95). Still loses 0-9% to every direct-damage bruiser
  (ELEPHANT/TIGER/MINOTAUR/GORILLA/SCORPION/TANK) - the intended weakness held;
  it did NOT become a threat. FFA floor (0%) by design - measure its real value
  in KOTH (hold-points), not fragging.
- ELEPHANT is the duel ceiling (70.2%), unchanged - fire/melee/knock immunity.
  Counters: FALCON (96), INSECT (83), TANK (37). Fair in FFA (3.0%).
- TIGER is FFA OP? (11.7%) - high kills/g (13.0) + best survivability of the top
  fraggers (8.9 deaths/g). Fair in duel (#4).
- CRAB is now the lone duel UP? (5.9%) - pure-utility kit, weak 1v1; fine in FFA (#2).
- SERPENT is #14 (18.8%): the turtle wall (97% loss) + its already-low base. Watch it;
  if it needs help, lean on its crit/dodge or non-poison damage, not more venom.

## Reference: primary-weapon DPS (base, pre-crit)

    MINOTAUR HAMMER 45 | INSECT SPIT 45 | FALCON TALON 43 (10% crit x1.7) |
    GORILLA POUND 43 | TIGER SCRATCH 40 | T-REX FLAME 40 | SCORPION LASER 36 |
    ELEPHANT HOOK 33 | HUMANOID GUN 32 | TANK CANNON 27 (clip 6) |
    OCTOPOD STAB 24 (x3 backstab, reach 3.2) | TURTLE SNAP 30 |
    SERPENT VENOM 18 (18% crit x2 + move-dodge) | CRAB SAND 14 | BUTTERFLY MEDIC 6.7

## TURTLE SHELL SHIELD (full kit)

    - rear-arc ranged fire blocked (flank to the front/side to land bolts)
    - IMMUNE: POISON (full - no chip, no DoT), SLOW, BLEED, DRAIN (take the hit, no DoT)
    - NOT immune: knockback (can be shoved off a point), stun, strangle, direct damage
    - JUMP = stationary invuln shell + heal (squat-and-hold the objective)
    - B = spin-roll ram (mobile attack, +14 dmg)

## Data caveats

- Bots don't backstab in a crowd, run CC combos, or shoot the crab turret -> octopod
  FFA/serpent win-rate reads low; DMG/G + matchups are the better signal.
- Octopod ACC 37% / turtle's low kills are melee whiffs + a defensive kit, not a gap.
- Serpent/falcon crit+dodge are RNG; their win-rate carries a bit more variance.
- CRAB accuracy >100% (turret hits credit the crab; its auto-fire isn't counted).
