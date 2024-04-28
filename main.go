package main

import (
	"encoding/json"
	"fmt"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

type FuelType int

const (
	Gas FuelType = iota // iota generates sequence of integers for each element
	Diesel
	LPG
	Electric
)

var fuelTypes = []FuelType{Gas, Diesel, LPG, Electric}

type TimeRange struct {
	Min, Max float32
}

type Config struct {
	FuelPricing       [4]float32   `json:"fuel_pricing"`
	FuelTypeChance    [4]float32   `json:"fuel_type_chance"`
	FuelingTime       [4]TimeRange `json:"fueling_time"`
	StationCounts     [4]int       `json:"station_counts"`
	CashRegisterCount int          `json:"cash_register_count"`

	CheckoutTime TimeRange `json:"checkout_time"`

	CarSpawnChance  float32 `json:"car_spawn_chance"` // checks 10 times a second
	CarWaitTimeBias float32 `json:"car_wait_time_bias"`

	SimulationLength time.Duration `json:"simulation_length"` // in seconds
}

var (
	GasStationCh      = make(chan Station)
	DieselStationCh   = make(chan Station)
	LPGStationCh      = make(chan Station)
	ElectricStationCh = make(chan Station)

	carChannel          = make(chan Car)
	checkoutChannel     = make(chan Car, 10)
	cashRegisterChannel = make(chan CashRegister)

	doneCh = make(chan bool) // finish sim channel

	ticker = time.NewTicker(100 * time.Millisecond) // 10 times a second

	stats = new(Stats)
	mu    = new(sync.Mutex)
)

func atomicAddFloat32(variable *float32, value float32) {
	mu.Lock()
	defer mu.Unlock()

	*variable += value
}

var config Config

func main() {
	config = *loadConfig()

	GasStationCh = make(chan Station, config.StationCounts[0])
	DieselStationCh = make(chan Station, config.StationCounts[1])
	LPGStationCh = make(chan Station, config.StationCounts[2])
	ElectricStationCh = make(chan Station, config.StationCounts[3])
	cashRegisterChannel = make(chan CashRegister, config.CashRegisterCount)

	go spawnCars()
	go manageGasStation()
	go printCurrentStats()

	time.Sleep(config.SimulationLength * time.Second)
	ticker.Stop()
	doneCh <- true

	// wait for finishing routines
	time.Sleep(200 * time.Millisecond)

	fmt.Println("-----------------------------------------------------------------")
	fmt.Println("Total cars: ", stats.CarsSpawnedTotal)
	fmt.Println("Cars refueled total: ", sumArray(stats.CarsRefueled))
	fmt.Println("Cars refueled by fuel type: ", stats.CarsRefueled)
	fmt.Println("Cars checked out total: ", sumArray(stats.CarsCheckedOut))
	fmt.Println("Cars not served: ", stats.CarsNotServed)
	fmt.Printf("Cars checked out rate: %.2f %%\n", sumArray(stats.CarsCheckedOut)/float32(stats.CarsSpawnedTotal)*100)
	fmt.Printf("Cars not served rate: %.2f %%\n", float32(stats.CarsNotServed)/float32(stats.CarsSpawnedTotal)*100)
	fmt.Println("-------------------------------")
	fmt.Printf("Average receipt: %.2f €\n", sumArray(stats.CashPerFuel)/sumArray(stats.CarsCheckedOut))
	fmt.Printf("Average receipt Gas: %.2f €\n", stats.CashPerFuel[0]/float32(stats.CarsCheckedOut[0]))
	fmt.Printf("Average receipt Diesel: %.2f €\n", stats.CashPerFuel[1]/float32(stats.CarsCheckedOut[1]))
	fmt.Printf("Average receipt LPG: %.2f €\n", stats.CashPerFuel[2]/float32(stats.CarsCheckedOut[2]))
	fmt.Printf("Average receipt Electric: %.2f €\n", stats.CashPerFuel[3]/float32(stats.CarsCheckedOut[3]))
	fmt.Println("-------------------------------")
	fmt.Printf("Average units refueled: %.2f\n", sumArray(stats.UnitsPerFuel)/sumArray(stats.CarsCheckedOut))
	fmt.Printf("Average liters of Gas: %.2f l\n", stats.UnitsPerFuel[0]/float32(stats.CarsCheckedOut[0]))
	fmt.Printf("Average liters of Diesel: %.2f l\n", stats.UnitsPerFuel[1]/float32(stats.CarsCheckedOut[1]))
	fmt.Printf("Average kilograms of LPG: %.2f kg\n", stats.UnitsPerFuel[2]/float32(stats.CarsCheckedOut[2]))
	fmt.Printf("Average kilowatt-hours recharged: %.2f kWh\n", stats.UnitsPerFuel[3]/float32(stats.CarsCheckedOut[3]))
	fmt.Println("-------------------------------")
	fmt.Printf("Average time spent refueling: %.2f s\n", sumArray(stats.TimeRefueling)/sumArray(stats.CarsRefueled))
	fmt.Printf("Average time spent GAS: %.2f s\n", stats.TimeRefueling[0]/float32(stats.CarsRefueled[0]))
	fmt.Printf("Average time spent Diesel: %.2f s\n", stats.TimeRefueling[1]/float32(stats.CarsRefueled[1]))
	fmt.Printf("Average time spent LPG: %.2f s\n", stats.TimeRefueling[2]/float32(stats.CarsRefueled[2]))
	fmt.Printf("Average time spent electric: %.2f s\n", stats.TimeRefueling[3]/float32(stats.CarsRefueled[3]))
	fmt.Printf("Average time spent checking out: %.2f s\n", stats.CheckoutTimeTotal/sumArray(stats.CarsCheckedOut))
	fmt.Printf("Average time spent in queue before leaving: %.2f s\n", stats.TimeBeforeLeaving/float32(stats.CarsNotServed))
	fmt.Printf("Average time spent at gas station: %.2f s\n", (sumArray(stats.TimeRefueling)+stats.CheckoutTimeTotal+float32(stats.TimeInCheckoutQueue))/sumArray(stats.CarsCheckedOut))
	fmt.Println("-----------------------------------------------------------------")
}

func checkoutCar(cashReg CashRegister) {
	// take out the car
	car := <-checkoutChannel
	atomic.AddInt32(&stats.CarsInCheckoutQueue, -1)
	atomicAddFloat32(&stats.TimeInCheckoutQueue, float32(time.Since(car.CheckoutQueueStart).Milliseconds())/1000.0)

	checkoutTime := config.CheckoutTime.Min + (rand.Float32() * (config.CheckoutTime.Max - config.CheckoutTime.Min))
	atomicAddFloat32(&stats.CheckoutTimeTotal, checkoutTime)
	atomicAddFloat32(&stats.CashPerFuel[car.Fuel], car.Receipt)

	//fmt.Printf("Checking out car ID: %v, at cash register ID: %v, for %vs %v\n", car.ID, cashReg.ID, checkoutTime, time.Now())
	time.Sleep(time.Duration(checkoutTime*1000) * time.Millisecond)

	atomic.AddInt32(&stats.CarsCheckedOut[car.Fuel], 1)
	cashRegisterChannel <- cashReg
}

func refuelCar(car Car) {
	// car is waiting for a station to free up
	atomic.AddInt32(&stats.CarsInRefuelQueue, 1)

	// assign correct station
	select {
	case station := <-getStationCh(car.Fuel):
		// car moves from queue to station
		atomic.AddInt32(&stats.CarsInRefuelQueue, -1)
		// refuel the car for random time within bounds
		refuelTime := station.FuelingTime.Min + (rand.Float32() * (station.FuelingTime.Max - station.FuelingTime.Min))
		//fmt.Printf("Refueling car ID: %v, fuel type: %v, for %vs\n", car.ID, getFuelTypeName(car.Fuel), refuelTime)
		time.Sleep(time.Duration(refuelTime*1000) * time.Millisecond)

		// calculate price of fuel
		units := (float32(refuelTime) / float32(station.FuelingTime.Max)) * float32(car.FuelTankSize)
		price := units * config.FuelPricing[car.Fuel]
		car.Receipt = price

		// stats
		atomicAddFloat32(&stats.UnitsPerFuel[car.Fuel], units)
		atomicAddFloat32(&stats.TimeRefueling[car.Fuel], refuelTime)
		atomic.AddInt32(&stats.CarsRefueled[car.Fuel], 1)

		// forward car to checkout queue
		car.CheckoutQueueStart = time.Now()
		checkoutChannel <- car
		atomic.AddInt32(&stats.CarsInCheckoutQueue, 1)

		// return station back to channel
		getStationCh(station.Fuel) <- station
	case <-time.After(time.Second * time.Duration(car.WaitTime)):
		// car left without refueling
		atomicAddFloat32(&stats.TimeBeforeLeaving, car.WaitTime)
		atomic.AddInt32(&stats.CarsNotServed, 1)
		atomic.AddInt32(&stats.CarsInRefuelQueue, -1)
	}
}

func manageGasStation() {
	// spawn stations
	id := 0
	for i := 0; i < len(config.StationCounts); i++ {
		for j := 0; j < config.StationCounts[i]; j++ {
			getStationCh(fuelTypes[i]) <- *NewStation(id, fuelTypes[i], config.FuelingTime[i])
			id++
		}
	}

	id = 0
	for i := 0; i < config.CashRegisterCount; i++ {
		cashRegisterChannel <- *NewCashRegister(id)
		id++
	}

	for {
		select {
		case car := <-carChannel:
			go refuelCar(car)
		case cashReg := <-cashRegisterChannel:
			go checkoutCar(cashReg)
		}
	}
}

func spawnCars() {
	for {
		select {
		case <-ticker.C:
			if rand.Float32() < config.CarSpawnChance {
				carChannel <- *NewCar(getFuelTypeByChance(), config.CarWaitTimeBias)
				atomic.AddInt32(&stats.CarsSpawnedTotal, 1)
			}
		case <-doneCh:
			return
		}
	}
}

func printCurrentStats() {
	tick := 1
	for {
		select {
		case <-ticker.C:
			if tick%10 == 0 {
				fmt.Println("Cars spawned: ", stats.CarsSpawnedTotal)
				fmt.Println("Cars in queue to refuel: ", stats.CarsInRefuelQueue)
				fmt.Println("Cars in queue to checkout: ", stats.CarsInCheckoutQueue)
				fmt.Println("Cars checked out: ", sumArray(stats.CarsCheckedOut))
			}
			tick++
		case <-doneCh:
			return
		}
	}
}

func loadConfig() *Config {
	jsonBytes, err := os.ReadFile("config.json")
	if err != nil {
		fmt.Println("Error reading config file:", err)
		return nil
	}

	var config Config
	err = json.Unmarshal(jsonBytes, &config)
	if err != nil {
		fmt.Println("Error unmarshalling JSON:", err)
		return nil
	}

	return &config
}

var carID = 0

func NewCar(fuel FuelType, waitTimeBias float32) *Car {
	c := new(Car)
	c.Fuel = fuel
	c.ID = carID
	carID++

	min := waitTimeBias / 1.5
	max := waitTimeBias * 2
	c.WaitTime = min + (rand.Float32() * (max - min))

	if fuel == Gas {
		c.FuelTankSize = (rand.Intn(17) + 8) * 5 // 40-120 l
	} else if fuel == Diesel {
		c.FuelTankSize = (rand.Intn(21) + 9) * 5 // 45-150 l
	} else if fuel == LPG {
		c.FuelTankSize = (rand.Intn(18) + 7) * 5 // 35-120 kg
	} else if fuel == Electric {
		c.FuelTankSize = (rand.Intn(19) + 6) * 5 // 30-120 kWh
	}

	return c
}

func NewStation(id int, fuel FuelType, time TimeRange) *Station {
	s := new(Station)
	s.ID = id
	s.Fuel = fuel
	s.FuelingTime = time

	return s
}

func NewCashRegister(id int) *CashRegister {
	c := new(CashRegister)
	c.ID = id

	return c
}

func getStationCh(fuel FuelType) chan Station {
	switch fuel {
	case Gas:
		return GasStationCh
	case Diesel:
		return DieselStationCh
	case LPG:
		return LPGStationCh
	case Electric:
		return ElectricStationCh
	default:
		return nil
	}
}

func getFuelTypeByChance() FuelType {
	var ranges [4][2]float32
	var total float32 = 0.0
	for i := range config.FuelTypeChance {
		ranges[i][0] = total
		ranges[i][1] = total + config.FuelTypeChance[i]
		total += config.FuelTypeChance[i]
	}

	probability := rand.Float32()

	var selected int = 0
	for i := range ranges {
		if probability >= ranges[i][0] && probability <= ranges[i][1] {
			selected = i
		}
	}

	return fuelTypes[selected]
}

func getFuelTypeName(fuel FuelType) string {
	switch fuel {
	case Gas:
		return "Gas"
	case Diesel:
		return "Diesel"
	case LPG:
		return "LPG"
	case Electric:
		return "Electric"
	default:
		return "None"
	}
}

type Car struct {
	ID                 int
	Fuel               FuelType
	WaitTime           float32 // max waiting time when all pumps busy
	FuelTankSize       int     // liters/kg/kwh
	Receipt            float32
	CheckoutQueueStart time.Time
}

type Station struct {
	ID          int
	Fuel        FuelType
	FuelingTime TimeRange
}

type CashRegister struct {
	ID int
}

type Stats struct {
	// car counts
	CarsSpawnedTotal    int32
	CarsNotServed       int32
	CarsRefueled        [4]int32
	CarsCheckedOut      [4]int32
	CarsInRefuelQueue   int32
	CarsInCheckoutQueue int32

	// money
	CashPerFuel       [4]float32
	CheckoutTimeTotal float32

	// fuel
	UnitsPerFuel  [4]float32
	TimeRefueling [4]float32

	// general time
	TimeBeforeLeaving   float32
	TimeInCheckoutQueue float32
}

func sumArray(arr interface{}) float32 {
	var sum float32
	switch arr := arr.(type) {
	case [4]int32:
		for _, v := range arr {
			sum += float32(v)
		}
	case [4]float32:
		for _, v := range arr {
			sum += v
		}
	}
	return sum
}
