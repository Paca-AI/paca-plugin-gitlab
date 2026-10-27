package main

import gp "github.com/Paca-AI/plugin-sdk-go/gitprovider"

// Compile-time proof that the provider implements the SPI and exactly the
// optional interfaces Describe declares (the conformance suite checks the
// declarations match).
var (
	_ gp.Provider          = (*glProvider)(nil)
	_ gp.ThreadResolver    = (*glProvider)(nil)
	_ gp.FileReader        = (*glProvider)(nil)
	_ gp.CodeSearcher      = (*glProvider)(nil)
	_ gp.LabelManager      = (*glProvider)(nil)
	_ gp.ReviewerRequester = (*glProvider)(nil)
	_ gp.AssigneeSetter    = (*glProvider)(nil)
	_ gp.Merger            = (*glProvider)(nil)
	_ gp.CheckRerunner     = (*glProvider)(nil)
	_ gp.WebhookManager    = (*glProvider)(nil)
	_ gp.IdentityDeleter   = (*glProvider)(nil)
	_ gp.BranchManager     = (*glProvider)(nil)
)
