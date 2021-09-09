package mobile

import "testing"

func TestStartMesh(t *testing.T) {
	ygg := &Mesh{}
	if err := ygg.StartAutoconfigure(); err != nil {
		t.Fatalf("Failed to start Mesh: %s", err)
	}
	t.Log("Address:", ygg.GetAddressString())
	t.Log("Subnet:", ygg.GetSubnetString())
	t.Log("Coords:", ygg.GetCoordsString())
	if err := ygg.Stop(); err != nil {
		t.Fatalf("Failed to stop Mesh: %s", err)
	}
}
